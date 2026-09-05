package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// principalOf inserts a principal of the given kind to act as.
func (g *graph) principalOf(ctx context.Context, kind string) domain.Actor {
	GinkgoHelper()
	var actor domain.Actor
	g.given(ctx, func(ctx context.Context, tx store.Tx) {
		p, err := tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: g.org, Kind: kind})
		Expect(err).NotTo(HaveOccurred())
		actor = domain.Actor{PrincipalID: p.ID, PrincipalKind: p.Kind, IdentityScheme: "unix"}
	})
	return actor
}

// eventsFor reads back everything one request wrote, as the log orders it.
func (g *graph) eventsFor(ctx context.Context, requestID string) []store.Event {
	GinkgoHelper()
	var events []store.Event
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		events, err = domain.EventsForRequest(ctx, tx, g.org, requestID)
		return err
	})).To(Succeed())
	return events
}

func (g *graph) humanTouched(ctx context.Context, ref string) bool {
	GinkgoHelper()
	var touched bool
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		touched, err = domain.HumanTouched(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return touched
}

func drop(node, reason string) domain.Op {
	return domain.Op{DropNode: &domain.DropNode{Node: node, Reason: reason}}
}

func reopen(node, reason string) domain.Op {
	return domain.Op{ReopenNode: &domain.ReopenNode{Node: node, Reason: reason}}
}

// ops renders events as the operation names they record, in order.
func ops(events []store.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Op)
	}
	return out
}

var _ = Describe("Event emission", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		actor domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		actor = g.principal(ctx)
	})

	Describe("Scenarios", func() {
		It("A patch is reconstructable from its events", func(ctx SpecContext) {
			By("given a patch of six operations")
			title := "A, retitled"
			patch := []domain.Op{
				create("a", "A"),
				create("b", "B"),
				addEdge("a", "b"),
				{UpdateNode: &domain.UpdateNode{Node: "a", Title: &title}},
				{RemoveEdge: &domain.RemoveEdge{From: "a", To: "b"}},
				drop("b", domain.DropNotViable),
			}
			r := g.mustApply(ctx, actor, "req-1", patch...)
			Expect(r.Events).To(HaveLen(6))

			By("when I read the events for that request id")
			events := g.eventsFor(ctx, "req-1")

			By("then six events exist in operation order")
			Expect(events).To(HaveLen(6))
			Expect(ops(events)).To(Equal([]string{
				domain.OpCreateNode, domain.OpCreateNode, domain.OpAddEdge,
				domain.OpUpdateNode, domain.OpRemoveEdge, domain.OpDropNode,
			}))
			Expect(events).To(Equal(r.Events), "the log reads back exactly what the patch returned")
			Expect(events).To(HaveEach(HaveField("RequestID", HaveValue(Equal("req-1")))))
			Expect(events[3].NodeID).To(HaveValue(Equal(g.id("#1"))))
			Expect(field(events[3].Before, "Title")).To(Equal("A"))
			Expect(field(events[3].After, "Title")).To(Equal("A, retitled"))
			Expect(events[5].Reason).To(HaveValue(Equal("not_viable")))
			Expect(field(events[5].After, "Status")).To(Equal("dropped"))
		})

		It("A client cannot forge an author", func(ctx SpecContext) {
			By("given an agent holding a lease on node 1")
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			agent := g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim", 0))

			By("and a request whose body sets a principal field")
			forged := g.principalOf(ctx, "user")
			body := `{
				"request_id": "req-1",
				"project": "platform",
				"lease_id": "` + held.Lease.ID + `",
				"principal_id": "` + forged.PrincipalID + `",
				"principal_kind": "user",
				"identity_scheme": "oidc",
				"ops": [{"create_node": {"title": "B", "parent": "#1", "principal_id": "` + forged.PrincipalID + `"}}]
			}`
			var patch domain.Patch
			Expect(json.Unmarshal([]byte(body), &patch)).To(Succeed())

			By("when the patch is applied")
			r, err := domain.Apply(ctx, s, pol, g.org, agent, patch)
			Expect(err).NotTo(HaveOccurred())
			g.ids["platform#2"] = r.Nodes[0].ID

			By("then the event records the transport-resolved principal")
			events := g.eventsFor(ctx, "req-1")
			Expect(events).To(HaveLen(1))
			Expect(events[0].PrincipalID).To(Equal(agent.PrincipalID))
			Expect(events[0].PrincipalKind).To(Equal("agent"))
			Expect(events[0].IdentityScheme).To(Equal("unix"))

			By("and the body's value is ignored")
			Expect(events[0].PrincipalID).NotTo(Equal(forged.PrincipalID))
			Expect(g.humanTouched(ctx, "#2")).To(BeFalse(), "a forged user principal leaves no human trace")
		})

		It("Dropping requires a reason", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))

			By("when a patch drops a node with no reason")
			_, err := g.apply(ctx, actor, "req-1", drop("#1", ""))

			By("then the patch is rejected")
			Expect(err).To(MatchError(domain.ErrReasonRequired))
			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Op).To(Equal(domain.OpDropNode))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
			Expect(g.countEvents(ctx, "req-1")).To(BeZero())
			_, err = s.Queries().GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{OrgID: g.org, RequestID: "req-1"})
			Expect(err).To(MatchError(ContainSubstring("no rows")), "a rejected patch claims no request id")
		})
	})

	Describe("One event per operation", func() {
		It("writes exactly one event for every operation, including transitions", func(ctx SpecContext) {
			r := g.mustApply(ctx, actor, "req-1",
				create("a", "A"),
				create("b", "B"),
				drop("a", domain.DropSuperseded),
				reopen("a", "review found it was needed"),
				addEdge("a", "b"),
			)
			Expect(r.Events).To(HaveLen(5))
			Expect(g.countEvents(ctx, "req-1")).To(Equal(5))
			Expect(ops(g.eventsFor(ctx, "req-1"))).To(Equal([]string{
				domain.OpCreateNode, domain.OpCreateNode, domain.OpDropNode, domain.OpReopenNode, domain.OpAddEdge,
			}))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
		})

		It("expands blocked_by into one add_edge event per blocker", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "expand",
				create("a", "A"), create("b", "B"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "c", Title: "C", BlockedBy: []string{"a", "b"}}},
			)
			Expect(ops(g.eventsFor(ctx, "expand"))).To(Equal([]string{
				domain.OpCreateNode, domain.OpCreateNode, domain.OpCreateNode, domain.OpAddEdge, domain.OpAddEdge,
			}))
		})

		It("records the reason, before, and after of a transition", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))

			r := g.mustApply(ctx, actor, "drop", domain.Op{DropNode: &domain.DropNode{
				Node: "#1", Reason: domain.DropOther, Detail: "folded into #2",
			}})
			Expect(r.Events[0].Reason).To(HaveValue(Equal("other: folded into #2")))
			Expect(field(r.Events[0].Before, "Status")).To(Equal("open"))
			Expect(field(r.Events[0].After, "Status")).To(Equal("dropped"))
			Expect(r.Nodes).To(HaveLen(1))
			Expect(r.Nodes[0].Status).To(Equal("dropped"))

			r = g.mustApply(ctx, actor, "reopen", reopen("#1", "not folded after all"))
			Expect(r.Events[0].Reason).To(HaveValue(Equal("not folded after all")))
			Expect(field(r.Events[0].Before, "Status")).To(Equal("dropped"))
			Expect(field(r.Events[0].After, "Status")).To(Equal("open"))
		})

		It("rejects dropping a dropped node and reopening an open one", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))

			_, err := g.apply(ctx, actor, "reopen", reopen("#1", "why"))
			Expect(err).To(MatchError(ContainSubstring("is open")))

			g.mustApply(ctx, actor, "drop", drop("#1", domain.DropSuperseded))
			_, err = g.apply(ctx, actor, "again", drop("#1", domain.DropSuperseded))
			Expect(err).To(MatchError(ContainSubstring("already dropped")))
		})
	})

	Describe("Reasons", func() {
		It("rejects a reopen without a reason", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), drop("a", domain.DropSuperseded))

			_, err := g.apply(ctx, actor, "req-1", reopen("#1", ""))

			Expect(err).To(MatchError(domain.ErrReasonRequired))
			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Op).To(Equal(domain.OpReopenNode))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("dropped"))
		})

		It("rejects a drop for another reason with no detail, and an unknown reason", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))

			_, err := g.apply(ctx, actor, "other", drop("#1", domain.DropOther))
			Expect(err).To(MatchError(domain.ErrReasonRequired))

			_, err = g.apply(ctx, actor, "unknown", drop("#1", "bored"))
			Expect(err).To(MatchError(ContainSubstring(`unknown drop reason "bored"`)))
			Expect(g.getNode(ctx, "#1").Status).To(Equal("open"))
		})

		It("rejects a bad transition before the whole patch, writing nothing", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "req-1", create("a", "A"), create("b", "B"), drop("a", ""))

			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Index).To(Equal(2))
			Expect(g.countNodes(ctx)).To(BeZero())
			Expect(g.countEvents(ctx, "req-1")).To(BeZero())
		})
	})

	Describe("Provenance", func() {
		It("refuses a patch with no actor", func(ctx SpecContext) {
			for _, a := range []domain.Actor{
				{},
				{PrincipalID: actor.PrincipalID, PrincipalKind: "agent"},
				{PrincipalID: actor.PrincipalID, IdentityScheme: "unix"},
			} {
				_, err := g.apply(ctx, a, "req-1", create("a", "A"))
				Expect(err).To(MatchError(domain.ErrNoActor))
			}
			Expect(g.countNodes(ctx)).To(BeZero())
		})

		It("has no field on a patch for the actor to arrive through", func() {
			var fields []string
			for _, doc := range []any{domain.Patch{}, domain.Op{}, domain.CreateNode{}, domain.UpdateNode{}, domain.DropNode{}, domain.ReopenNode{}} {
				encoded, err := json.Marshal(doc)
				Expect(err).NotTo(HaveOccurred())
				var m map[string]json.RawMessage
				Expect(json.Unmarshal(encoded, &m)).To(Succeed())
				for k := range m {
					fields = append(fields, k)
				}
			}
			Expect(fields).NotTo(ContainElements(
				ContainSubstring("principal"), ContainSubstring("identity"), ContainSubstring("actor"), ContainSubstring("author"),
			))
		})

		It("records the identity scheme the actor arrived with", func(ctx SpecContext) {
			viaTailnet := actor
			viaTailnet.IdentityScheme = "tailscale"
			r := g.mustApply(ctx, viaTailnet, "req-1", create("a", "A"))
			Expect(r.Events[0].IdentityScheme).To(Equal("tailscale"))
			Expect(r.Events[0].PrincipalID).To(Equal(actor.PrincipalID))
		})
	})

	Describe("human_touched", func() {
		It("is false until a user principal writes to the node, then true", func(ctx SpecContext) {
			By("given a node an agent created under the one it holds")
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			agent := g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim", 0))
			_, err := g.applyWithLease(ctx, agent, "by-agent", held.Lease.ID,
				domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", Parent: "#1"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.humanTouched(ctx, "#2")).To(BeFalse())

			title := "B, by its agent"
			_, err = g.applyWithLease(ctx, agent, "by-agent-again", held.Lease.ID,
				domain.Op{UpdateNode: &domain.UpdateNode{Node: "#2", Title: &title}})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.humanTouched(ctx, "#2")).To(BeFalse(), "only a user principal counts")

			g.mustApply(ctx, actor, "by-user", drop("#2", domain.DropNoLongerRelevant))
			Expect(g.humanTouched(ctx, "#2")).To(BeTrue())

			_, err = g.applyWithLease(ctx, agent, "reopen", held.Lease.ID, reopen("#2", "still needed"))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.humanTouched(ctx, "#2")).To(BeTrue(), "a later agent write does not erase the human's")
		})

		It("counts an edge a user added into the node, and not the edge's source", func(ctx SpecContext) {
			By("given nodes A and B an agent created under the one it holds")
			g.mustApply(ctx, actor, "setup", create("root", "Root"))
			agent := g.agent(ctx)
			held := g.mustClaim(ctx, agent, claimReq("claim", 0))
			_, err := g.applyWithLease(ctx, agent, "children", held.Lease.ID, childOf("a", "A", "#1"), childOf("b", "B", "#1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.humanTouched(ctx, "#2")).To(BeFalse())
			Expect(g.humanTouched(ctx, "#3")).To(BeFalse())

			g.mustApply(ctx, actor, "link", addEdge("#2", "#3"))

			Expect(g.humanTouched(ctx, "#3")).To(BeTrue(), "the waiting side is what changed")
			Expect(g.humanTouched(ctx, "#2")).To(BeFalse())
		})
	})
})

var _ = Describe("Event ids", func() {
	It("are minted in the order they are asked for", func() {
		ids := make([]string, 0, 5000)
		for range cap(ids) {
			ids = append(ids, domain.NewULID())
		}
		Expect(sort.StringsAreSorted(ids)).To(BeTrue(), "ids minted in sequence sort in sequence")
		seen := map[string]bool{}
		for _, id := range ids {
			Expect(seen[id]).To(BeFalse(), "duplicate id %s", id)
			seen[id] = true
			Expect(id).To(HaveLen(26))
		}
	})
})
