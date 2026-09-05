package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// principal inserts a user principal to act as: the person laying out the
// work, whom the default policy lets edit anything in the organization.
func (g *graph) principal(ctx context.Context) domain.Actor {
	GinkgoHelper()
	return g.principalOf(ctx, "user")
}

// agent inserts an agent principal, which the default policy confines to
// the node it holds a lease on and what it creates under it.
func (g *graph) agent(ctx context.Context) domain.Actor {
	GinkgoHelper()
	return g.principalOf(ctx, "agent")
}

// apply runs one patch against the spec's project and records every node
// it returns under its reference, so later steps can name them.
func (g *graph) apply(ctx context.Context, actor domain.Actor, requestID string, ops ...domain.Op) (*domain.Result, error) {
	GinkgoHelper()
	return g.applyPatch(ctx, actor, domain.Patch{RequestID: requestID, Project: "platform", Ops: ops})
}

// applyPatch runs one patch as given and records every node it returns
// under its reference.
func (g *graph) applyPatch(ctx context.Context, actor domain.Actor, p domain.Patch) (*domain.Result, error) {
	GinkgoHelper()
	r, err := domain.Apply(ctx, g.s, pol, g.org, actor, p)
	if r != nil {
		for _, n := range r.Nodes {
			g.ids[fmt.Sprintf("platform#%d", n.Number)] = n.ID
		}
	}
	return r, err
}

// mustApply is apply for fixtures.
func (g *graph) mustApply(ctx context.Context, actor domain.Actor, requestID string, ops ...domain.Op) *domain.Result {
	GinkgoHelper()
	r, err := g.apply(ctx, actor, requestID, ops...)
	Expect(err).NotTo(HaveOccurred())
	return r
}

// create is a create_node operation with a title and an alias.
func create(alias, title string) domain.Op {
	return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: title}}
}

func addEdge(from, to string) domain.Op {
	return domain.Op{AddEdge: &domain.AddEdge{From: from, To: to}}
}

// holdLock takes the organization's graph lock from a session of its own
// and returns the function that releases it.
func (g *graph) holdLock(ctx context.Context) func() {
	GinkgoHelper()
	conn, err := g.s.Pool().Acquire(ctx)
	Expect(err).NotTo(HaveOccurred())
	tx, err := conn.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, domain.LockNamespace, g.org)
	Expect(err).NotTo(HaveOccurred())
	return func() {
		Expect(tx.Rollback(ctx)).To(Succeed())
		conn.Release()
	}
}

// applyAsync runs a patch in the background and delivers its outcome.
func (g *graph) applyAsync(ctx context.Context, actor domain.Actor, requestID string, ops ...domain.Op) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		_, err := domain.Apply(ctx, g.s, pol, g.org, actor, domain.Patch{RequestID: requestID, Project: "platform", Ops: ops})
		done <- err
	}()
	return done
}

// field reads one top-level field of a JSON document, such as an event's
// before or after.
func field(doc []byte, key string) any {
	GinkgoHelper()
	var m map[string]any
	Expect(json.Unmarshal(doc, &m)).To(Succeed())
	return m[key]
}

func (g *graph) countEvents(ctx context.Context, requestID string) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE org_id = $1 AND request_id = $2`, g.org, requestID).Scan(&n)).To(Succeed())
	return n
}

func (g *graph) countEdges(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx, `SELECT count(*) FROM edges WHERE org_id = $1`, g.org).Scan(&n)).To(Succeed())
	return n
}

func (g *graph) nodeCounter(ctx context.Context) int64 {
	GinkgoHelper()
	p, err := g.s.Queries().GetProject(ctx, store.GetProjectParams{OrgID: g.org, ID: g.project})
	Expect(err).NotTo(HaveOccurred())
	return p.NodeCounter
}

func (g *graph) getNode(ctx context.Context, ref string) store.Node {
	GinkgoHelper()
	n, err := g.s.Queries().GetNode(ctx, store.GetNodeParams{OrgID: g.org, ID: g.id(ref)})
	Expect(err).NotTo(HaveOccurred())
	return n
}

// acyclic reports whether the wait relation through the given nodes has
// no cycle, as the cycle check judges it.
func (g *graph) acyclic(ctx context.Context, refs ...string) bool {
	GinkgoHelper()
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, g.id(r))
	}
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return domain.CheckAcyclic(ctx, tx, g.org, ids)
	})
	if err == nil {
		return true
	}
	Expect(asCycle(err)).NotTo(BeNil())
	return false
}

var _ = Describe("Patch application", Label(testsupport.DBLabel), func() {
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
		It("Forward references resolve within one patch", func(ctx SpecContext) {
			By("given an empty project")
			Expect(g.countNodes(ctx)).To(BeZero())

			By("when a patch creates node A, creates node B blocked_by A, and adds an edge from B to A's parent")
			r, err := g.apply(ctx, actor, "req-1",
				create("p", "Parent of A"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "a", Title: "A", Parent: "p"}},
				domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}},
				addEdge("b", "p"),
			)
			Expect(err).NotTo(HaveOccurred())

			By("then all operations commit together")
			Expect(g.countNodes(ctx)).To(Equal(3))
			Expect(g.countEdges(ctx)).To(Equal(2))
			Expect(r.Aliases).To(Equal(map[string]string{"p": "platform#1", "a": "platform#2", "b": "platform#3"}))
			Expect(numbers(r.Nodes)).To(Equal([]int64{1, 2, 3}))
			Expect(g.getNode(ctx, "#2").ParentID).To(HaveValue(Equal(g.id("#1"))))
			Expect(r.Events).To(HaveLen(5))
			Expect(r.Events).To(HaveEach(SatisfyAll(
				HaveField("RequestID", HaveValue(Equal("req-1"))),
				HaveField("PrincipalID", actor.PrincipalID),
				HaveField("PrincipalKind", "user"),
				HaveField("IdentityScheme", "unix"),
			)))
			Expect(g.hasEdge(ctx, "#3", "#1")).To(BeTrue())

			By("and B is blocked by A")
			Expect(g.hasEdge(ctx, "#2", "#3")).To(BeTrue())
			Expect(g.explain(ctx, "#3").Reasons).To(ConsistOf(BeReason(domain.ReasonBlocked, 2)))
		})

		It("Two concurrent patches cannot form a cycle between them", func(ctx SpecContext) {
			By("given nodes A and B with no edges")
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
			Expect(g.countEdges(ctx)).To(BeZero())

			By("when one patch adds an edge from A to B and another adds an edge from B to A concurrently")
			release := g.holdLock(ctx)
			ab := g.applyAsync(ctx, actor, "a-to-b", addEdge("#1", "#2"))
			ba := g.applyAsync(ctx, actor, "b-to-a", addEdge("#2", "#1"))
			Consistently(ab, 300*time.Millisecond).ShouldNot(Receive())
			Consistently(ba, 300*time.Millisecond).ShouldNot(Receive())
			release()
			var abErr, baErr error
			Eventually(ab, 5*time.Second).Should(Receive(&abErr))
			Eventually(ba, 5*time.Second).Should(Receive(&baErr))

			By("then exactly one commits")
			Expect([]error{abErr, baErr}).To(ContainElement(BeNil()))
			Expect(g.countEdges(ctx)).To(Equal(1))
			Expect(g.countEvents(ctx, "a-to-b") + g.countEvents(ctx, "b-to-a")).To(Equal(1))

			By("and the other is rejected")
			var rejected error
			if abErr == nil {
				rejected = baErr
			} else {
				rejected = abErr
			}
			Expect(asCycle(rejected)).NotTo(BeNil())

			By("and the graph is acyclic")
			Expect(g.acyclic(ctx, "#1", "#2")).To(BeTrue())
		})

		It("A crash mid-patch leaves no half graph", func(ctx SpecContext) {
			By("given a patch creating five nodes where the fourth violates a constraint")
			patch := []domain.Op{
				create("one", "One"), create("two", "Two"), create("three", "Three"),
				create("four", ""),
				create("five", "Five"),
			}

			By("when the patch is applied")
			r, err := g.apply(ctx, actor, "req-1", patch...)

			By("then no node from that patch exists")
			Expect(r).To(BeNil())
			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Index).To(Equal(3))
			Expect(opErr.Op).To(Equal(domain.OpCreateNode))
			Expect(g.countNodes(ctx)).To(BeZero())
			Expect(g.countEvents(ctx, "req-1")).To(BeZero())
			Expect(g.nodeCounter(ctx)).To(Equal(int64(1)), "the number block is released with the rest")
			_, err = s.Queries().GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{OrgID: g.org, RequestID: "req-1"})
			Expect(err).To(MatchError(pgx.ErrNoRows), "a failed patch stores no response to replay")
		})

		It("A retried patch does not duplicate work", func(ctx SpecContext) {
			By("given a patch that committed but whose response was lost")
			patch := []domain.Op{create("a", "A"), {CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}}}
			first := g.mustApply(ctx, actor, "req-1", patch...)
			nodes, events := g.countNodes(ctx), g.countEvents(ctx, "req-1")
			Expect(nodes).To(Equal(2))
			Expect(events).To(Equal(3))

			By("when the client retries with the same request id and body")
			again, err := g.apply(ctx, actor, "req-1", patch...)
			Expect(err).NotTo(HaveOccurred())

			By("then the original response is returned")
			Expect(again.Replayed).To(BeTrue())
			again.Replayed = false
			Expect(again).To(Equal(first))

			By("and no additional nodes or events exist")
			Expect(g.countNodes(ctx)).To(Equal(nodes))
			Expect(g.countEvents(ctx, "req-1")).To(Equal(events))
		})
	})

	Describe("Idempotency", func() {
		It("rejects the same request id with different content as a conflict", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "req-1", create("a", "A"))

			_, err := g.apply(ctx, actor, "req-1", create("a", "A"), create("b", "B"))

			var conflict *domain.RequestConflictError
			Expect(errors.As(err, &conflict)).To(BeTrue(), "expected a request conflict, got %v", err)
			Expect(conflict.RequestID).To(Equal("req-1"))
			Expect(err.Error()).To(Equal(`request "req-1" was already applied with different content`))
			Expect(g.countNodes(ctx)).To(Equal(1))
			Expect(g.countEvents(ctx, "req-1")).To(Equal(1))
		})

		It("replays for the loser when two applications of one request race", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))

			release := g.holdLock(ctx)
			first := g.applyAsync(ctx, actor, "req-1", addEdge("#1", "#2"))
			second := g.applyAsync(ctx, actor, "req-1", addEdge("#1", "#2"))
			Consistently(first, 200*time.Millisecond).ShouldNot(Receive())
			release()
			Eventually(first, 5*time.Second).Should(Receive(BeNil()))
			Eventually(second, 5*time.Second).Should(Receive(BeNil()))

			Expect(g.countEdges(ctx)).To(Equal(1))
			Expect(g.countEvents(ctx, "req-1")).To(Equal(1))
			Expect(g.countEvents(ctx, "setup")).To(Equal(2))
		})

		It("requires a request id", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "", create("a", "A"))
			Expect(err).To(MatchError(domain.ErrNoRequestID))
		})
	})

	Describe("Advisory lock", func() {
		It("is held by a patch that adds an edge until it commits", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
			release := g.holdLock(ctx)

			done := g.applyAsync(ctx, actor, "edge", addEdge("#1", "#2"))
			Consistently(done, 300*time.Millisecond).ShouldNot(Receive(), "an edge patch waits for the organization lock")

			release()
			Eventually(done, 5*time.Second).Should(Receive(BeNil()))
			Expect(g.hasEdge(ctx, "#1", "#2")).To(BeTrue())
		})

		It("is taken by a patch that changes a parent", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), create("b", "B"))
			release := g.holdLock(ctx)

			parent := "#1"
			done := g.applyAsync(ctx, actor, "reparent", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#2", Parent: &parent}})
			Consistently(done, 300*time.Millisecond).ShouldNot(Receive())

			release()
			Eventually(done, 5*time.Second).Should(Receive(BeNil()))
			Expect(g.getNode(ctx, "#2").ParentID).To(HaveValue(Equal(g.id("#1"))))
		})

		It("Patches touching only node fields take no advisory lock", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"))
			release := g.holdLock(ctx)
			defer release()

			title := "A, retitled"
			done := g.applyAsync(ctx, actor, "fields",
				create("b", "B"),
				domain.Op{UpdateNode: &domain.UpdateNode{Node: "#1", Title: &title, Labels: &[]string{"x"}}},
			)
			Eventually(done, 5*time.Second).Should(Receive(BeNil()), "a node-only patch commits while the organization lock is held elsewhere")
			Expect(g.countNodes(ctx)).To(Equal(2))
		})
	})

	Describe("Operations", func() {
		It("update_node changes only the fields that are set", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", domain.Op{CreateNode: &domain.CreateNode{
				Alias: "a", Title: "A", Body: "body", Kind: "bug", Priority: "p1", Labels: []string{"x", "y"},
			}})
			before := g.getNode(ctx, "#1")

			body := "new body"
			r := g.mustApply(ctx, actor, "update", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#1", Body: &body, Labels: &[]string{}}})

			after := g.getNode(ctx, "#1")
			Expect(after.Body).To(Equal("new body"))
			Expect(after.Labels).To(BeEmpty())
			Expect(after.Kind).To(Equal(before.Kind))
			Expect(after.Priority).To(Equal(before.Priority))
			Expect(after.Title).To(Equal(before.Title))
			Expect(r.Nodes).To(HaveLen(1))
			Expect(r.Events).To(HaveLen(1))
			Expect(r.Events[0].Op).To(Equal(domain.OpUpdateNode))
			Expect(field(r.Events[0].Before, "Body")).To(Equal("body"))
			Expect(field(r.Events[0].After, "Body")).To(Equal("new body"))
		})

		It("update_node can make a node a root again", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", Parent: "a"}})
			Expect(g.getNode(ctx, "#2").ParentID).NotTo(BeNil())

			root := ""
			g.mustApply(ctx, actor, "unparent", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#2", Parent: &root}})

			Expect(g.getNode(ctx, "#2").ParentID).To(BeNil())
		})

		It("remove_edge removes the edge and records it", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("a", "A"), domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}})
			Expect(g.hasEdge(ctx, "#1", "#2")).To(BeTrue())

			r := g.mustApply(ctx, actor, "unlink", domain.Op{RemoveEdge: &domain.RemoveEdge{From: "#1", To: "#2"}})

			Expect(g.hasEdge(ctx, "#1", "#2")).To(BeFalse())
			Expect(r.Events).To(HaveLen(1))
			Expect(r.Events[0].Op).To(Equal(domain.OpRemoveEdge))
			Expect(r.Events[0].NodeID).To(HaveValue(Equal(g.id("#2"))))
			Expect(r.Nodes).To(HaveLen(1), "the target is reported, since its readiness changed")

			_, err := g.apply(ctx, actor, "again", domain.Op{RemoveEdge: &domain.RemoveEdge{From: "#1", To: "#2"}})
			Expect(err).To(MatchError(ContainSubstring("no blocks edge from #1 to #2")))
		})

		It("rejects a patch whose operations form a cycle, creating nothing", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "req-1",
				create("a", "A"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "b", Title: "B", BlockedBy: []string{"a"}}},
				addEdge("b", "a"),
			)
			Expect(refs(asCycle(err))).To(Equal([]string{"platform#1", "platform#2", "platform#1"}))
			Expect(g.countNodes(ctx)).To(BeZero())
			Expect(g.countEvents(ctx, "req-1")).To(BeZero())
		})

		It("names the operation whose reference does not resolve", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "req-1", create("a", "A"), addEdge("a", "missing"))

			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Index).To(Equal(1))
			Expect(opErr.Op).To(Equal(domain.OpAddEdge))
			Expect(err).To(MatchError(domain.ErrNodeNotFound))
			Expect(g.countNodes(ctx)).To(BeZero())
		})

		It("rejects an empty patch and an empty operation", func(ctx SpecContext) {
			_, err := g.apply(ctx, actor, "req-1")
			Expect(err).To(MatchError(domain.ErrNoOperations))

			_, err = g.apply(ctx, actor, "req-2", domain.Op{})
			var opErr *domain.OpError
			Expect(errors.As(err, &opErr)).To(BeTrue(), "expected an operation error, got %v", err)
			Expect(opErr.Index).To(BeZero())
		})

		It("resolves the project by id, slug, or org/slug, and refuses an archived one", func(ctx SpecContext) {
			for i, project := range []string{g.project, "platform", "acme/platform"} {
				_, err := domain.Apply(ctx, s, pol, g.org, actor, domain.Patch{RequestID: fmt.Sprintf("req-%d", i), Project: project, Ops: []domain.Op{create("", "A")}})
				Expect(err).NotTo(HaveOccurred(), "project %q", project)
			}
			Expect(g.countNodes(ctx)).To(Equal(3))

			_, err := domain.Apply(ctx, s, pol, g.org, actor, domain.Patch{RequestID: "missing", Project: "nope", Ops: []domain.Op{create("", "A")}})
			Expect(err).To(MatchError(domain.ErrProjectNotFound))

			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.archive(ctx, tx, g.project) })
			_, err = g.apply(ctx, actor, "archived", create("", "A"))
			Expect(err).To(MatchError(domain.ErrProjectArchived))
		})
	})
})
