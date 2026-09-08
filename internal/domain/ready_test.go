package domain_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// nodeOpts are the fields of a fixture node that readiness cares about.
// Zero values take the schema's defaults: open, task, p2, no assignee.
type nodeOpts struct {
	parent       *string
	status       string
	kind         string
	priority     string
	assigneeKind string
	labels       []string
	project      string // project id; the spec's project when empty
	slug         string // the project's slug; "platform" when empty
}

// nodeWith inserts node `number` with the given options.
func (g *graph) nodeWith(ctx context.Context, tx store.Tx, number int64, o nodeOpts) {
	GinkgoHelper()
	project, slug := g.project, "platform"
	if o.project != "" {
		project, slug = o.project, o.slug
	}
	p := store.CreateNodeParams{
		ID: newULID(), Number: number, OrgID: g.org, ProjectID: project, ParentID: o.parent,
		Kind: "task", Title: fmt.Sprintf("Node %d", number), Priority: "p2", Labels: []string{},
	}
	if o.kind != "" {
		p.Kind = o.kind
	}
	if o.priority != "" {
		p.Priority = o.priority
	}
	if o.assigneeKind != "" {
		p.AssigneeKind = &o.assigneeKind
	}
	if o.labels != nil {
		p.Labels = o.labels
	}
	n, err := tx.CreateNode(ctx, p)
	Expect(err).NotTo(HaveOccurred())
	g.ids[fmt.Sprintf("%s#%d", slug, number)] = n.ID
	if o.status != "" {
		_, err := tx.SetNodeStatus(ctx, store.SetNodeStatusParams{OrgID: g.org, ID: n.ID, Status: o.status})
		Expect(err).NotTo(HaveOccurred())
	}
}

// gate inserts a user-assigned node.
func (g *graph) gate(ctx context.Context, tx store.Tx, number int64) {
	GinkgoHelper()
	g.nodeWith(ctx, tx, number, nodeOpts{assigneeKind: domain.AssigneeUser})
}

func (g *graph) setStatus(ctx context.Context, tx store.Tx, ref, status string) {
	GinkgoHelper()
	_, err := tx.SetNodeStatus(ctx, store.SetNodeStatusParams{OrgID: g.org, ID: g.id(ref), Status: status})
	Expect(err).NotTo(HaveOccurred())
}

// lease claims a node for an agent principal, expiring after ttl. A
// negative ttl backdates the whole lease so its TTL has already passed.
func (g *graph) lease(ctx context.Context, tx store.Tx, ref string, ttl time.Duration) store.Lease {
	GinkgoHelper()
	holder, err := tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: g.org, Kind: "agent"})
	Expect(err).NotTo(HaveOccurred())
	l, err := tx.CreateLease(ctx, store.CreateLeaseParams{
		ID: newULID(), OrgID: g.org, NodeID: g.id(ref), Holder: holder.ID,
		Harness: "spec", AgentRef: "spec", ExpiresAt: time.Now().Add(max(ttl, time.Hour)),
	})
	Expect(err).NotTo(HaveOccurred())
	if ttl < 0 {
		claimed, expires := time.Now().Add(2*ttl), time.Now().Add(ttl)
		_, err := tx.Tx.Exec(ctx, `UPDATE leases SET claimed_at = $2, expires_at = $3 WHERE id = $1`, l.ID, claimed, expires)
		Expect(err).NotTo(HaveOccurred())
		l.ClaimedAt, l.ExpiresAt = claimed, expires
	}
	return l
}

func (g *graph) release(ctx context.Context, tx store.Tx, l store.Lease, reason string) {
	GinkgoHelper()
	_, err := tx.ReleaseLease(ctx, store.ReleaseLeaseParams{OrgID: g.org, ID: l.ID, ReleaseReason: &reason})
	Expect(err).NotTo(HaveOccurred())
}

func (g *graph) archive(ctx context.Context, tx store.Tx, project string) {
	GinkgoHelper()
	_, err := tx.ArchiveProject(ctx, store.ArchiveProjectParams{OrgID: g.org, ID: project})
	Expect(err).NotTo(HaveOccurred())
}

// ready runs the ordered ready query over the spec's project.
func (g *graph) ready(ctx context.Context, q domain.ReadyQuery) []store.Node {
	GinkgoHelper()
	if q.Projects == nil {
		q.Projects = []string{g.project}
	}
	var nodes []store.Node
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		nodes, err = domain.Ready(ctx, tx, g.org, q)
		return err
	})).To(Succeed())
	return nodes
}

// readySet runs the unordered ready query.
func (g *graph) readySet(ctx context.Context, q domain.ReadyQuery) ([]store.Node, error) {
	GinkgoHelper()
	var nodes []store.Node
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		nodes, err = domain.ReadySet(ctx, tx, g.org, q)
		return err
	})
	return nodes, err
}

func (g *graph) explain(ctx context.Context, ref string) *domain.Explanation {
	GinkgoHelper()
	var e *domain.Explanation
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		e, err = domain.Explain(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return e
}

// numbers renders nodes as their numbers, in order.
func numbers(nodes []store.Node) []int64 {
	out := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Number)
	}
	return out
}

// BeReason matches a Reason by kind and, when given, by the number of the
// node it names.
func BeReason(kind domain.ReasonKind, number ...int64) OmegaMatcher {
	m := HaveField("Kind", kind)
	if len(number) == 0 {
		return m
	}
	var field string
	switch kind {
	case domain.ReasonBlocked:
		field = "Blocker.Number"
	case domain.ReasonOpenChild:
		field = "Child.Number"
	case domain.ReasonHeld:
		field = "Hold.Number"
	default:
		panic("BeReason: " + string(kind) + " names no node")
	}
	return And(m, HaveField(field, number[0]))
}

var _ = Describe("Readiness", Label(testsupport.DBLabel), func() {
	var (
		s *store.Store
		g *graph
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
	})

	Describe("Scenarios", func() {
		It("A grouping node is not work", func(ctx SpecContext) {
			By("given node 1 with two open children")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &one)
				g.node(ctx, tx, 3, &one)
			})

			By("when I query the ready set")
			ready := g.ready(ctx, domain.ReadyQuery{})

			By("then node 1 is not ready")
			Expect(numbers(ready)).To(ConsistOf(int64(2), int64(3)))

			By("and Explain says it has open children")
			e := g.explain(ctx, "#1")
			Expect(e.Ready()).To(BeFalse())
			Expect(e.Reasons).To(HaveExactElements(
				BeReason(domain.ReasonOpenChild, 2),
				BeReason(domain.ReasonOpenChild, 3),
			))
			Expect(e.Reasons[0].String()).To(Equal("its child #2 is open"))
		})

		It("A grouping node becomes claimable when its children close", func(ctx SpecContext) {
			By("given node 1 whose children are all done")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.nodeWith(ctx, tx, 2, nodeOpts{parent: &one, status: "done"})
				g.nodeWith(ctx, tx, 3, nodeOpts{parent: &one, status: "done"})
			})

			By("then node 1 is ready")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
			Expect(g.explain(ctx, "#1").Ready()).To(BeTrue())
		})

		It("A dropped prerequisite unblocks its dependents", func(ctx SpecContext) {
			By("given node 2 is blocked by node 1")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))

			By("when node 1 is dropped without cascade")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.setStatus(ctx, tx, "#1", "dropped")
			})

			By("then node 2 is ready")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{2}))
			Expect(g.explain(ctx, "#2").Ready()).To(BeTrue())
		})

		It("A dropped gate does not unblock", func(ctx SpecContext) {
			By("given node 2 is blocked by gate node 1")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.gate(ctx, tx, 1)
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})

			By("when node 1 is dropped")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.setStatus(ctx, tx, "#1", "dropped")
			})

			By("then node 2 is not ready")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty())

			By("and Explain still names node 1")
			e := g.explain(ctx, "#2")
			Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonBlocked, 1)))
			Expect(e.Reasons[0].Gate).To(BeTrue())
			Expect(e.Reasons[0].Blocker.Status).To(Equal("dropped"))
			Expect(e.Reasons[0].String()).To(Equal("it is blocked by dropped gate #1"))
		})

		It("An unassigned node is agent work", func(ctx SpecContext) {
			By("given an open node with no assignee_kind")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
			})

			By("when an agent queries the ready set with no filters")
			ready := g.ready(ctx, domain.ReadyQuery{})

			By("then the node is returned")
			Expect(numbers(ready)).To(Equal([]int64{1}))
			Expect(ready[0].AssigneeKind).To(BeNil())

			By("and it is returned when the agent filters to agent work")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{AssigneeKind: domain.AssigneeAgent}))).To(Equal([]int64{1}))
		})

		It("Explain answers why not", func(ctx SpecContext) {
			By("given a node blocked by two open nodes and one user-assigned node")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
				g.gate(ctx, tx, 3)
				g.node(ctx, tx, 4, nil)
				g.edge(ctx, tx, "#1", "#4")
				g.edge(ctx, tx, "#2", "#4")
				g.edge(ctx, tx, "#3", "#4")
			})

			By("when I call Explain")
			e := g.explain(ctx, "#4")

			By("then three structured reasons are returned")
			Expect(e.Reasons).To(HaveExactElements(
				BeReason(domain.ReasonBlocked, 1),
				BeReason(domain.ReasonBlocked, 2),
				BeReason(domain.ReasonBlocked, 3),
			))
			Expect(e.Reasons[0].Gate).To(BeFalse())
			Expect(e.Reasons[1].Gate).To(BeFalse())
			Expect(e.Reasons[2].Gate).To(BeTrue(), "the user-assigned blocker is a gate: the node waits on a person")
			Expect(e.Reasons[2].String()).To(Equal("it is blocked by open gate #3"))
			Expect(e.Stuck()).To(BeFalse())
		})
	})

	Describe("A node is ready when open, unblocked, unleased, and has no open children", func() {
		It("returns an open node nothing waits on", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
		})

		It("excludes a done or dropped node, and Explain says so", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{status: "done"})
				g.nodeWith(ctx, tx, 2, nodeOpts{status: "dropped"})
			})
			Expect(g.ready(ctx, domain.ReadyQuery{})).To(BeEmpty())

			done := g.explain(ctx, "#1")
			Expect(done.Reasons).To(HaveExactElements(And(BeReason(domain.ReasonNotOpen), HaveField("Status", "done"))))
			Expect(done.Reasons[0].Permanent).To(BeFalse(), "done can be reopened")
			Expect(done.Reasons[0].String()).To(Equal("it is done"))

			dropped := g.explain(ctx, "#2")
			Expect(dropped.Reasons).To(HaveExactElements(And(BeReason(domain.ReasonNotOpen), HaveField("Status", "dropped"))))
			Expect(dropped.Reasons[0].Permanent).To(BeTrue(), "dropped is terminal")
			Expect(dropped.Stuck()).To(BeFalse(), "a dropped node is a state, not a bug")
		})

		It("excludes a node with an open blocking predecessor", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
			Expect(g.explain(ctx, "#2").Reasons).To(HaveExactElements(BeReason(domain.ReasonBlocked, 1)))
		})

		It("ignores edges of a kind other than blocks", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
				_, err := tx.CreateEdge(ctx, store.CreateEdgeParams{OrgID: g.org, FromNode: g.id("#1"), ToNode: g.id("#2"), Kind: "relates"})
				Expect(err).NotTo(HaveOccurred())
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1), int64(2)))
		})

		It("excludes a node under a live lease, and Explain returns the lease with its expiry", func(ctx SpecContext) {
			var l store.Lease
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				l = g.lease(ctx, tx, "#1", time.Hour)
			})
			Expect(g.ready(ctx, domain.ReadyQuery{})).To(BeEmpty())

			e := g.explain(ctx, "#1")
			Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonLeased)))
			Expect(e.Reasons[0].Lease.ID).To(Equal(l.ID))
			Expect(e.Reasons[0].Lease.Holder).To(Equal(l.Holder))
			Expect(e.Reasons[0].Lease.ExpiresAt).To(BeTemporally("~", l.ExpiresAt, time.Millisecond))
			Expect(e.Reasons[0].String()).To(HavePrefix("it is held by lease " + l.ID + " until "))
		})

		It("returns a node whose lease has expired or been released", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.lease(ctx, tx, "#1", -time.Minute)
				g.node(ctx, tx, 2, nil)
				g.release(ctx, tx, g.lease(ctx, tx, "#2", time.Hour), "crash")
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1), int64(2)))
		})

		It("excludes a node whose project is archived, and Explain names the project", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.archive(ctx, tx, g.project)
			})
			Expect(g.ready(ctx, domain.ReadyQuery{})).To(BeEmpty())

			e := g.explain(ctx, "#1")
			Expect(e.Reasons).To(HaveExactElements(And(BeReason(domain.ReasonArchived), HaveField("Project.Slug", "platform"))))
			Expect(e.Reasons[0].String()).To(Equal("its project platform is archived"))
		})

		It("counts children by parent, not by edge, so a done child no longer holds its parent", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.nodeWith(ctx, tx, 2, nodeOpts{parent: &one, status: "done"})
				g.nodeWith(ctx, tx, 3, nodeOpts{parent: &one, status: "dropped"})
				g.node(ctx, tx, 4, &one)
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{4}))
			Expect(g.explain(ctx, "#1").Reasons).To(HaveExactElements(BeReason(domain.ReasonOpenChild, 4)))
		})
	})

	Describe("A done or dropped predecessor satisfies a blocking edge, except a gate", func() {
		It("is satisfied by a done predecessor", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{status: "done"})
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{2}))
		})

		It("is satisfied by a completed gate", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{assigneeKind: domain.AssigneeUser, status: "done"})
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{2}))
			Expect(g.explain(ctx, "#2").Ready()).To(BeTrue())
		})

		It("is not satisfied by an open gate", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.gate(ctx, tx, 1)
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			By("the gate itself is ready, so a notifier can find it")
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{1}))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{AssigneeKind: domain.AssigneeUser}))).To(Equal([]int64{1}))
		})

		It("names a blocker in another project by its own project", func(ctx SpecContext) {
			spike := g.newProject(ctx, "spike")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{project: spike, slug: "spike"})
				g.node(ctx, tx, 1, nil)
				g.edge(ctx, tx, "spike#1", "platform#1")
			})
			e := g.explain(ctx, "platform#1")
			Expect(e.Reasons).To(HaveExactElements(And(BeReason(domain.ReasonBlocked, 1), HaveField("Blocker.ProjectID", spike))))
		})
	})

	Describe("Unset assignee_kind is agent-eligible, never excluded from the ready set", func() {
		BeforeEach(func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.nodeWith(ctx, tx, 2, nodeOpts{assigneeKind: domain.AssigneeAgent})
				g.gate(ctx, tx, 3)
			})
		})

		It("returns unset and agent-assigned nodes to an agent filter", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{AssigneeKind: domain.AssigneeAgent}))).To(ConsistOf(int64(1), int64(2)))
		})

		It("returns only gates to a user filter", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{AssigneeKind: domain.AssigneeUser}))).To(Equal([]int64{3}))
		})

		It("returns everything to no filter", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1), int64(2), int64(3)))
		})

		It("rejects an assignee kind that is neither", func(ctx SpecContext) {
			_, err := g.readySet(ctx, domain.ReadyQuery{AssigneeKind: "service"})
			Expect(err).To(MatchError(ContainSubstring(`assignee kind "service"`)))
		})
	})

	Describe("An ordered query requires exactly one project", func() {
		var spike string

		BeforeEach(func(ctx SpecContext) {
			spike = g.newProject(ctx, "spike")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.nodeWith(ctx, tx, 1, nodeOpts{project: spike, slug: "spike", priority: "p0"})
			})
		})

		It("rejects an ordered query naming no project", func(ctx SpecContext) {
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Ready(ctx, tx, g.org, domain.ReadyQuery{})
				return err
			})).To(MatchError(domain.ErrOneProject))
		})

		It("rejects an ordered query naming two projects", func(ctx SpecContext) {
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Ready(ctx, tx, g.org, domain.ReadyQuery{Projects: []string{g.project, spike}})
				return err
			})).To(MatchError(domain.ErrOneProject))
		})

		It("answers an ordered query for each project on its own", func(ctx SpecContext) {
			Expect(g.ready(ctx, domain.ReadyQuery{Projects: []string{g.project}})).To(HaveEach(HaveField("ProjectID", g.project)))
			Expect(g.ready(ctx, domain.ReadyQuery{Projects: []string{spike}})).To(HaveEach(HaveField("ProjectID", spike)))
		})

		It("answers an unordered query across the organization", func(ctx SpecContext) {
			nodes, err := g.readySet(ctx, domain.ReadyQuery{})
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).To(HaveLen(2))
			Expect(nodes).To(ContainElements(HaveField("ProjectID", g.project), HaveField("ProjectID", spike)))
		})
	})

	Describe("Ordering by priority then age", func() {
		It("returns the most urgent first, then the oldest", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{priority: "p2"})
				g.nodeWith(ctx, tx, 2, nodeOpts{priority: "p0"})
				g.nodeWith(ctx, tx, 3, nodeOpts{priority: "p2"})
				g.nodeWith(ctx, tx, 4, nodeOpts{priority: "p1"})
				// Age is created_at, not number: make 3 older than 1.
				for number, age := range map[int64]string{1: "2 hours", 2: "1 hour", 3: "3 hours", 4: "1 hour"} {
					_, err := tx.Tx.Exec(ctx, `UPDATE nodes SET created_at = now() - $2::interval WHERE org_id = $1 AND number = $3`, g.org, age, number)
					Expect(err).NotTo(HaveOccurred())
				}
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(Equal([]int64{2, 4, 3, 1}))
		})

		It("caps the page at the limit", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{priority: "p3"})
				g.nodeWith(ctx, tx, 2, nodeOpts{priority: "p1"})
				g.nodeWith(ctx, tx, 3, nodeOpts{priority: "p2"})
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{Limit: 2}))).To(Equal([]int64{2, 3}))
		})
	})

	Describe("Filters", func() {
		BeforeEach(func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{kind: "bug", priority: "p0", labels: []string{"backend", "urgent"}})
				g.nodeWith(ctx, tx, 2, nodeOpts{kind: "task", priority: "p2", labels: []string{"backend"}})
				g.nodeWith(ctx, tx, 3, nodeOpts{kind: "spike", priority: "p4"})
			})
		})

		It("by kind", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{Kind: "bug"}))).To(Equal([]int64{1}))
			Expect(g.ready(ctx, domain.ReadyQuery{Kind: "chore"})).To(BeEmpty())
		})

		It("by every label given", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{Labels: []string{"backend"}}))).To(Equal([]int64{1, 2}))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{Labels: []string{"backend", "urgent"}}))).To(Equal([]int64{1}))
			Expect(g.ready(ctx, domain.ReadyQuery{Labels: []string{"frontend"}})).To(BeEmpty())
		})

		It("by minimum priority", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{MinPriority: "p2"}))).To(Equal([]int64{1, 2}))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{MinPriority: "p0"}))).To(Equal([]int64{1}))
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{MinPriority: "p4"}))).To(Equal([]int64{1, 2, 3}))
		})

		It("rejects a priority outside p0 to p4", func(ctx SpecContext) {
			_, err := g.readySet(ctx, domain.ReadyQuery{MinPriority: "high"})
			Expect(err).To(MatchError(ContainSubstring(`priority "high"`)))
		})

		It("combine", func(ctx SpecContext) {
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{Kind: "task", Labels: []string{"backend"}, MinPriority: "p2"}))).To(Equal([]int64{2}))
			Expect(g.ready(ctx, domain.ReadyQuery{Kind: "task", Labels: []string{"urgent"}})).To(BeEmpty())
		})
	})

	Describe("A node with no possible path to readiness is identifiable", func() {
		It("is stuck when a dropped gate blocks it", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.nodeWith(ctx, tx, 1, nodeOpts{assigneeKind: domain.AssigneeUser, status: "dropped"})
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			e := g.explain(ctx, "#2")
			Expect(e.Stuck()).To(BeTrue())
			Expect(e.Reasons).To(HaveExactElements(And(BeReason(domain.ReasonBlocked, 1), HaveField("Permanent", true))))
		})

		It("is not stuck while its blocker can still close", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.gate(ctx, tx, 1)
				g.node(ctx, tx, 2, nil)
				g.node(ctx, tx, 3, nil)
				g.edge(ctx, tx, "#1", "#3")
				g.edge(ctx, tx, "#2", "#3")
				g.lease(ctx, tx, "#3", time.Hour)
			})
			e := g.explain(ctx, "#3")
			Expect(e.Ready()).To(BeFalse())
			Expect(e.Stuck()).To(BeFalse())
			Expect(e.Reasons).To(HaveEach(HaveField("Permanent", false)))
		})

		It("is not stuck when it is ready", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			Expect(g.explain(ctx, "#1").Stuck()).To(BeFalse())
		})
	})

	Describe("Explain", func() {
		It("lists every failing condition, in a fixed order", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				two := g.node(ctx, tx, 2, nil)
				g.node(ctx, tx, 3, &two)
				g.edge(ctx, tx, "#1", "#2")
				g.lease(ctx, tx, "#2", time.Hour)
			})
			e := g.explain(ctx, "#2")
			Expect(e.Node.Number).To(Equal(int64(2)))
			Expect(e.Reasons).To(HaveExactElements(
				BeReason(domain.ReasonBlocked, 1),
				BeReason(domain.ReasonLeased),
				BeReason(domain.ReasonOpenChild, 3),
			))
		})

		It("reports an unknown node", func(ctx SpecContext) {
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Explain(ctx, tx, g.org, newULID())
				return err
			})).To(MatchError(domain.ErrNodeNotFound))
		})

		It("does not explain another organization's node", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })
			other := newGraph(ctx, s)
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Explain(ctx, tx, other.org, g.id("#1"))
				return err
			})).To(MatchError(domain.ErrNodeNotFound))
		})
	})
})
