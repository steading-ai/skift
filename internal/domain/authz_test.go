package domain_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// setCounter moves the project's node counter so the next created node
// takes the given number, letting a spec number its nodes the way the
// scenario does.
func (g *graph) setCounter(ctx context.Context, number int64) {
	GinkgoHelper()
	_, err := g.s.Pool().Exec(ctx, `UPDATE projects SET node_counter = $3 WHERE org_id = $1 AND id = $2`, g.org, g.project, number)
	Expect(err).NotTo(HaveOccurred())
}

// childOf is a create_node operation under a parent.
func childOf(alias, title, parent string) domain.Op {
	return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: title, Parent: parent}}
}

// gateOf is a create_node operation filing a gate under a parent.
func gateOf(alias, title, parent string) domain.Op {
	return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: title, Parent: parent, AssigneeKind: domain.AssigneeUser}}
}

// rootGate is a create_node operation filing a gate at the root of the
// project.
func rootGate(alias, title string) domain.Op {
	return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: title, AssigneeKind: domain.AssigneeUser}}
}

// assignedToAgent is a create_node operation filing agent-assigned work at
// the root of the project.
func assignedToAgent(alias, title string) domain.Op {
	return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: title, AssigneeKind: domain.AssigneeAgent}}
}

// tellsWhatToDo matches a refusal that names the gate and the next move.
func tellsWhatToDo(gateID string) OmegaMatcher {
	return SatisfyAll(
		MatchError(ContainSubstring("gate "+gateID)),
		MatchError(ContainSubstring("comment")),
		MatchError(ContainSubstring("release your lease with reason gate")),
	)
}

// BeDeniedAt matches a patch refused by policy at the given operation.
func BeDeniedAt(index int) OmegaMatcher {
	return SatisfyAll(
		MatchError(policy.ErrDenied),
		WithTransform(func(err error) int {
			var opErr *domain.OpError
			if !errors.As(err, &opErr) {
				return -1
			}
			return opErr.Index
		}, Equal(index)),
	)
}

// versioned matches events all carrying the default bundle's version.
func versioned() OmegaMatcher {
	return HaveEach(HaveField("PolicyVersion", pol.Version()))
}

var _ = Describe("Authorization", Label(testsupport.DBLabel), func() {
	var (
		s      *store.Store
		g      *graph
		user   domain.Actor
		agentA domain.Actor
		lease  store.Lease
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		user = g.principal(ctx)
		agentA = g.agent(ctx)

		By("given node 1 with children 7 and 8, laid out by a user")
		g.mustApply(ctx, user, "root", create("root", "Node 1"))
		g.setCounter(ctx, 7)
		g.mustApply(ctx, user, "children", childOf("seven", "Node 7", "#1"), childOf("eight", "Node 8", "#1"))
		Expect(g.getNode(ctx, "#8").ParentID).To(HaveValue(Equal(g.id("#1"))))

		By("and agent A holding a lease on node 7")
		held := g.mustClaim(ctx, agentA, claimReq("claim-7", 0))
		Expect(held.Node.Number).To(Equal(int64(7)))
		lease = held.Lease
	})

	// fileGate has agent A file gate node 9 under node 7, blocking node 7.
	fileGate := func(ctx context.Context) {
		GinkgoHelper()
		_, err := g.applyWithLease(ctx, agentA, "file-gate", lease.ID, gateOf("gate", "Need a credential", "#7"), addEdge("gate", "#7"))
		Expect(err).NotTo(HaveOccurred())
		Expect(g.getNode(ctx, "#9").AssigneeKind).To(HaveValue(Equal(domain.AssigneeUser)))
		Expect(g.explain(ctx, "#7").Reasons).To(ContainElement(BeReason(domain.ReasonBlocked, 9)))
	}

	Describe("Scenarios", func() {
		It("An agent cannot edit a sibling", func(ctx SpecContext) {
			By("when agent A patches node 8")
			_, err := g.applyWithLease(ctx, agentA, "sibling", lease.ID, update("#8", "Edited by A"))

			By("then the patch is denied")
			Expect(err).To(BeDeniedAt(0))
			Expect(err).To(MatchError(ContainSubstring("no rule permits agent update_node")))
			Expect(g.getNode(ctx, "#8").Title).To(Equal("Node 8"))
			Expect(g.countEvents(ctx, "sibling")).To(BeZero())

			By("and without its lease it may not even edit node 7")
			_, err = g.apply(ctx, agentA, "bare", update("#7", "Edited by A"))
			Expect(err).To(MatchError(domain.ErrLeaseRequired), "fencing refuses it before policy does")
			_, err = g.apply(ctx, agentA, "bare-8", update("#8", "Edited by A"))
			Expect(err).To(BeDeniedAt(0))
		})

		It("An agent can block itself", func(ctx SpecContext) {
			By("when it creates a node and adds an edge from it into node 7")
			r, err := g.applyWithLease(ctx, agentA, "block", lease.ID, childOf("pre", "Prerequisite", "#7"), addEdge("pre", "#7"))

			By("then the patch is allowed")
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Aliases).To(HaveKeyWithValue("pre", "platform#9"))
			Expect(g.hasEdge(ctx, "#9", "#7")).To(BeTrue())
			Expect(g.explain(ctx, "#7").Reasons).To(ContainElement(BeReason(domain.ReasonBlocked, 9)))
			Expect(r.Events).To(versioned())
		})

		It("An agent cannot bypass its own gate", func(ctx SpecContext) {
			By("given agent A filed gate node 9 blocking node 7")
			fileGate(ctx)

			By("when agent A drops node 9")
			_, err := g.applyWithLease(ctx, agentA, "drop-gate", lease.ID, drop("#9", domain.DropNoLongerRelevant))

			By("then the operation is denied")
			Expect(err).To(BeDeniedAt(0))
			Expect(err).To(MatchError(ContainSubstring("filed gate " + g.id("#9") + " may not drop it")))
			Expect(g.getNode(ctx, "#9").Status).To(Equal("open"))

			By("and when agent A reassigns, reclassifies, or unlinks node 9")
			agent := domain.AssigneeAgent
			_, err = g.applyWithLease(ctx, agentA, "reassign", lease.ID, domain.Op{UpdateNode: &domain.UpdateNode{Node: "#9", AssigneeKind: &agent}})
			Expect(err).To(BeDeniedAt(0))
			kind := "chore"
			_, err = g.applyWithLease(ctx, agentA, "reclassify", lease.ID, domain.Op{UpdateNode: &domain.UpdateNode{Node: "#9", Kind: &kind}})
			Expect(err).To(BeDeniedAt(0))
			_, err = g.applyWithLease(ctx, agentA, "unlink", lease.ID, domain.Op{RemoveEdge: &domain.RemoveEdge{From: "#9", To: "#7"}})
			Expect(err).To(BeDeniedAt(0))

			By("then node 7 stays blocked by its gate")
			Expect(g.hasEdge(ctx, "#9", "#7")).To(BeTrue())
			Expect(g.explain(ctx, "#7").Reasons).To(ContainElement(SatisfyAll(BeReason(domain.ReasonBlocked, 9), HaveField("Gate", true))))
		})

		It("A dropped gate is not approval", func(ctx SpecContext) {
			By("given agent A filed gate node 9 blocking node 7")
			fileGate(ctx)

			By(`when a user drops gate node 9 with reason "no longer relevant"`)
			r, err := g.apply(ctx, user, "drop-gate", drop("#9", domain.DropNoLongerRelevant))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Events).To(HaveLen(1))
			Expect(r.Events[0].Reason).To(HaveValue(Equal(domain.DropNoLongerRelevant)))

			By("then node 7 remains blocked")
			Expect(g.getNode(ctx, "#9").Status).To(Equal("dropped"))
			Expect(g.explain(ctx, "#7").Reasons).To(ContainElement(SatisfyAll(
				BeReason(domain.ReasonBlocked, 9), HaveField("Gate", true), HaveField("Permanent", true),
			)))
		})

		It("A user completes the gate it filed", func(ctx SpecContext) {
			By("given a gate filed by user principal U")
			g.setCounter(ctx, 9)
			g.mustApply(ctx, user, "file", rootGate("gate", "Mint the deploy token"), addEdge("gate", "#8"))
			Expect(g.explain(ctx, "#8").Reasons).To(ContainElement(BeReason(domain.ReasonBlocked, 9)))

			By("when U completes it")
			r, err := g.apply(ctx, user, "close", complete("#9", "Token minted and placed in the agent's environment"))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Events).To(HaveLen(1))

			By("then the node is done and the edges out of it are satisfied")
			Expect(g.getNode(ctx, "#9").Status).To(Equal("done"))
			Expect(g.hasEdge(ctx, "#9", "#8")).To(BeTrue())
			Expect(g.explain(ctx, "#8").Reasons).NotTo(ContainElement(BeReason(domain.ReasonBlocked, 9)))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ContainElement(int64(8)))

			By("and U may equally drop, reclassify, and unlink a gate of its own")
			g.mustApply(ctx, user, "second", rootGate("two", "Pay the invoice"), addEdge("two", "#8"))
			agent := domain.AssigneeAgent
			_, err = g.apply(ctx, user, "reclassify", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#10", AssigneeKind: &agent}})
			Expect(err).NotTo(HaveOccurred())
			kind := "chore"
			_, err = g.apply(ctx, user, "kind", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#10", Kind: &kind}})
			Expect(err).NotTo(HaveOccurred())
			_, err = g.apply(ctx, user, "unlink", domain.Op{RemoveEdge: &domain.RemoveEdge{From: "#10", To: "#8"}})
			Expect(err).NotTo(HaveOccurred())
			_, err = g.apply(ctx, user, "drop", drop("#10", domain.DropNoLongerRelevant))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.getNode(ctx, "#10").Status).To(Equal("dropped"))
		})

		It("An agent cannot escape a gate it filed", func(ctx SpecContext) {
			By("given a gate filed by agent principal A blocking A's own work")
			fileGate(ctx)
			id := g.id("#9")

			By("when A attempts to complete it, drop it, change its assignee_kind, or remove the edge")
			agent := domain.AssigneeAgent
			attempts := map[string]domain.Op{
				"escape-complete":   complete("#9", "I decided it myself"),
				"escape-drop":       drop("#9", domain.DropNoLongerRelevant),
				"escape-reclassify": {UpdateNode: &domain.UpdateNode{Node: "#9", AssigneeKind: &agent}},
				"escape-unlink":     {RemoveEdge: &domain.RemoveEdge{From: "#9", To: "#7"}},
			}

			By("then each attempt is refused with a message naming the gate and what to do instead")
			for request, op := range attempts {
				_, err := g.applyWithLease(ctx, agentA, request, lease.ID, op)
				Expect(err).To(BeDeniedAt(0), request)
				Expect(err).To(tellsWhatToDo(id), request)
			}
			Expect(g.getNode(ctx, "#9").Status).To(Equal("open"))
			Expect(g.getNode(ctx, "#9").AssigneeKind).To(HaveValue(Equal(domain.AssigneeUser)))
			Expect(g.hasEdge(ctx, "#9", "#7")).To(BeTrue())

			By("and it may not claim any gate, nor complete one it did not file")
			_, err := g.claim(ctx, agentA, domain.ClaimRequest{
				RequestID: "claim-gate", Project: "platform", AssigneeKind: domain.AssigneeUser, Harness: "spec", AgentRef: "spec",
			})
			Expect(err).To(MatchError(policy.ErrDenied))
			Expect(err).To(MatchError(ContainSubstring("an agent may not claim gate " + id)))

			g.mustApply(ctx, user, "another-gate", rootGate("theirs", "Pay the invoice"))
			_, err = g.apply(ctx, agentA, "outsider", complete("#10", "Done for you"))
			Expect(err).To(BeDeniedAt(0))
			Expect(err).To(MatchError(ContainSubstring("only a user may complete gate " + g.id("#10"))))
		})

		It("Human work depends on agent preparation", func(ctx SpecContext) {
			By(`given "Recommend a hosting provider" assigned to an agent`)
			g.setCounter(ctx, 9)
			g.mustApply(ctx, user, "prepare",
				assignedToAgent("recommend", "Recommend a hosting provider"),
				rootGate("signup", "Sign up with the chosen provider"),
				addEdge("recommend", "signup"))
			// The recommendation is the only p0 node, so the claim below
			// takes it rather than whatever else the project has ready.
			p0 := "p0"
			g.mustApply(ctx, user, "urgent", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#9", Priority: &p0}})

			By(`and gate "Sign up with the chosen provider" blocked by it`)
			Expect(g.getNode(ctx, "#10").AssigneeKind).To(HaveValue(Equal(domain.AssigneeUser)))
			Expect(g.explain(ctx, "#10").Reasons).To(ContainElement(BeReason(domain.ReasonBlocked, 9)))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{AssigneeKind: domain.AssigneeUser}))).NotTo(ContainElement(int64(10)))

			By("when the agent completes the recommendation")
			worker := g.agent(ctx)
			held := g.mustClaim(ctx, worker, domain.ClaimRequest{
				RequestID: "claim-9", Project: "platform", AssigneeKind: domain.AssigneeAgent,
				MinPriority: "p0", Harness: "spec", AgentRef: "spec",
			})
			Expect(held.Node.Number).To(Equal(int64(9)))
			const recommendation = "Use grove: cheapest at this size, and it already has the tailnet"
			_, err := g.applyWithLease(ctx, worker, "recommended", held.Lease.ID, complete("#9", recommendation))
			Expect(err).NotTo(HaveOccurred())

			By("then the gate is ready for the user and its inputs carry the recommendation")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{AssigneeKind: domain.AssigneeUser}))).To(ContainElement(int64(10)))
			Expect(g.explain(ctx, "#10").Reasons).To(BeEmpty())
			in := g.inputs(ctx, "#10")
			Expect(in).To(HaveLen(1))
			Expect(in[0].Result.Body).To(Equal(recommendation))
		})

		It("Policy failure is closed", func(ctx SpecContext) {
			By("given no policy bundle could be loaded")
			var none *policy.Bundle

			By("when any authorization is evaluated")
			_, err := domain.Apply(ctx, s, none, g.org, user, domain.Patch{RequestID: "no-policy", Project: "platform", Ops: []domain.Op{create("x", "X")}})

			By("then it is denied")
			Expect(err).To(BeDeniedAt(0))
			Expect(g.countNodes(ctx)).To(Equal(3))
			Expect(g.countEvents(ctx, "no-policy")).To(BeZero())
			_, err = domain.Claim(ctx, s, none, g.org, user, claimReq("no-policy-claim", 0))
			Expect(err).To(MatchError(policy.ErrDenied))
			_, err = domain.Heartbeat(ctx, s, none, g.org, agentA, "no-policy-beat", lease.ID, 0)
			Expect(err).To(MatchError(policy.ErrDenied))
		})
	})

	Describe("Edges", func() {
		It("out of a node require link permission on the target", func(ctx SpecContext) {
			_, err := g.applyWithLease(ctx, agentA, "out", lease.ID, addEdge("#7", "#8"))
			Expect(err).To(BeDeniedAt(0))
			Expect(g.hasEdge(ctx, "#7", "#8")).To(BeFalse())

			_, err = g.applyWithLease(ctx, agentA, "own", lease.ID, childOf("a", "A", "#7"), childOf("b", "B", "#7"), addEdge("a", "b"))
			Expect(err).NotTo(HaveOccurred(), "a node it created is a target it may link")
			Expect(g.hasEdge(ctx, "#9", "#10")).To(BeTrue())
		})

		It("across projects require write on the source and link on the target", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeIn(ctx, tx, g.newProject(ctx, "other"), "other", 1, nil)
			})
			_, err := g.applyWithLease(ctx, agentA, "cross", lease.ID, addEdge("other#1", "#7"))
			Expect(err).To(BeDeniedAt(0), "the holder may block itself within its project, but has no write on the other project's node")
			Expect(g.hasEdge(ctx, "other#1", "#7")).To(BeFalse())

			_, err = g.apply(ctx, user, "cross-by-user", addEdge("other#1", "#8"))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.hasEdge(ctx, "other#1", "#8")).To(BeTrue())
		})
	})

	Describe("Gates", func() {
		It("may be disposed of by a user who did not file them, but not by their filer", func(ctx SpecContext) {
			fileGate(ctx)
			filer := agentA

			By("the filer cannot reword the gate away from a gate, but can reword it")
			title := "Need the deploy credential, specifically"
			_, err := g.applyWithLease(ctx, filer, "reword", lease.ID, domain.Op{UpdateNode: &domain.UpdateNode{Node: "#9", Title: &title}})
			Expect(err).NotTo(HaveOccurred())

			By("once the filer has stepped back, another principal may unlink it")
			_, err = domain.Release(ctx, s, pol, g.org, filer, "step-back", lease.ID, domain.ReleaseGate, "")
			Expect(err).NotTo(HaveOccurred())
			_, err = g.apply(ctx, user, "unlink", domain.Op{RemoveEdge: &domain.RemoveEdge{From: "#9", To: "#7"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.hasEdge(ctx, "#9", "#7")).To(BeFalse())
		})
	})

	Describe("Bundle version", func() {
		It("is recorded on every event resulting from an authorized mutation", func(ctx SpecContext) {
			Expect(pol.Version()).NotTo(BeEmpty())

			By("on patch events")
			r, err := g.applyWithLease(ctx, agentA, "patch", lease.ID, childOf("c", "Child", "#7"), addEdge("c", "#7"), drop("c", domain.DropSuperseded), reopen("c", "needed"))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Events).To(HaveLen(4))
			Expect(r.Events).To(versioned())

			By("on lease events")
			beat, err := domain.Heartbeat(ctx, s, pol, g.org, agentA, "beat", lease.ID, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(beat.Events).To(versioned())
			released, err := domain.Release(ctx, s, pol, g.org, agentA, "release", lease.ID, domain.ReleaseGate, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(released.Events).To(versioned())

			agentB := g.agent(ctx)
			claimed := g.mustClaim(ctx, agentB, claimReq("claim-8", time.Minute))
			Expect(claimed.Node.Number).To(Equal(int64(8)))
			Expect(claimed.Events).To(versioned())

			By("and on expiry, whoever notices it")
			g.age(ctx, claimed.Lease.ID, 2*time.Minute, -time.Minute)
			service := g.principalOf(ctx, "service")
			expired, err := domain.ExpireLeases(ctx, s, pol, g.org, service, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(expired).To(HaveLen(1))
			events := g.nodeEvents(ctx, "#8")
			Expect(events[len(events)-1].Op).To(Equal(domain.OpExpireLease))
			Expect(events).To(versioned())

			By("and the column admits nothing else")
			var n int
			Expect(s.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE org_id = $1 AND policy_version <> $2`, g.org, pol.Version()).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		})
	})
})
