package domain_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// hold parks a node directly in the store, which is what the patch
// operation does once policy has allowed it.
func (g *graph) hold(ctx context.Context, tx store.Tx, ref, reason string) {
	GinkgoHelper()
	by := g.principalOf(ctx, "user").PrincipalID
	_, err := tx.HoldNode(ctx, store.HoldNodeParams{
		OrgID: g.org, ID: g.id(ref), HeldBy: &by, HeldReason: &reason,
	})
	Expect(err).NotTo(HaveOccurred())
}

// holdProject parks a whole project.
func (g *graph) holdProject(ctx context.Context, tx store.Tx, project, reason string) {
	GinkgoHelper()
	by := g.principalOf(ctx, "user").PrincipalID
	_, err := tx.HoldProject(ctx, store.HoldProjectParams{
		OrgID: g.org, ID: project, HeldBy: &by, HeldReason: &reason,
	})
	Expect(err).NotTo(HaveOccurred())
}

// heldCount is how many open nodes of the spec's project are parked.
func (g *graph) heldCount(ctx context.Context) int {
	GinkgoHelper()
	var held int
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		held, err = domain.HeldCount(ctx, tx, g.org, g.project)
		return err
	})).To(Succeed())
	return held
}

var _ = Describe("Holds", Label(testsupport.DBLabel), func() {
	var (
		s *store.Store
		g *graph
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
	})

	It("A held node leaves the ready set with its status untouched", func(ctx SpecContext) {
		By("given open nodes 1 and 2, and 1 is held")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.hold(ctx, tx, "#1", "not this quarter")
		})

		By("when I query the ready set")
		ready := g.ready(ctx, domain.ReadyQuery{})

		By("then only node 2 is ready")
		Expect(numbers(ready)).To(ConsistOf(int64(2)))

		By("and node 1 is still open")
		e := g.explain(ctx, "#1")
		Expect(e.Node.Status).To(Equal("open"))

		By("and Explain says who parked it and why, without calling it stuck")
		Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonHeld, 1)))
		Expect(e.Reasons[0].Hold.Self).To(BeTrue())
		Expect(e.Reasons[0].Hold.Reason).To(Equal("not this quarter"))
		Expect(e.Reasons[0].Hold.By).NotTo(BeEmpty())
		Expect(e.Stuck()).To(BeFalse())
	})

	It("Containment carries a hold, including to a node filed later", func(ctx SpecContext) {
		By("given epic 1 held, with child 2 under it")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			one := g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, &one)
			g.hold(ctx, tx, "#1", "parked")
		})

		By("then the child is not ready and Explain names the ancestor holding it")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty())
		e := g.explain(ctx, "#2")
		Expect(e.Reasons).To(HaveExactElements(BeReason(domain.ReasonHeld, 1)))
		Expect(e.Reasons[0].Hold.Self).To(BeFalse())

		By("when a grandchild is filed under the child afterwards")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			two := g.id("platform#2")
			g.node(ctx, tx, 3, &two)
		})

		By("then it is parked too, with no hold written to it")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty())
		Expect(g.explain(ctx, "#3").Reasons).To(HaveExactElements(BeReason(domain.ReasonHeld, 1)))
	})

	It("Explain names every hold in the way, nearest first", func(ctx SpecContext) {
		By("given a chain 1 > 2 > 3 with holds on 1 and 3")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			one := g.node(ctx, tx, 1, nil)
			two := g.node(ctx, tx, 2, &one)
			g.node(ctx, tx, 3, &two)
			g.hold(ctx, tx, "#1", "quarter")
			g.hold(ctx, tx, "#3", "flaky")
		})

		By("then both are reported, the node's own hold first")
		e := g.explain(ctx, "#3")
		Expect(e.Reasons).To(HaveExactElements(
			BeReason(domain.ReasonHeld, 3),
			BeReason(domain.ReasonHeld, 1),
		))
		Expect(e.Reasons[0].Hold.Self).To(BeTrue())
	})

	It("A held project parks its work and stays writable", func(ctx SpecContext) {
		By("given an open node in a held project")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.holdProject(ctx, tx, g.project, "not this quarter")
		})

		By("then nothing is ready and Explain names the project")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty())
		e := g.explain(ctx, "#1")
		Expect(e.Reasons).To(HaveExactElements(HaveField("Kind", domain.ReasonProjectHeld)))
		Expect(e.Reasons[0].Project.HeldReason).To(HaveValue(Equal("not this quarter")))
		Expect(e.Stuck()).To(BeFalse())

		By("and the count of parked work answers what an empty ready page owes")
		Expect(g.heldCount(ctx)).To(Equal(1))
	})

	It("A held node still blocks what depends on it", func(ctx SpecContext) {
		By("given node 1 blocking node 2, with 1 held")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.edge(ctx, tx, "#1", "#2")
			g.hold(ctx, tx, "#1", "parked")
		})

		By("then neither is ready, and 2 is blocked rather than held")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty())
		Expect(g.explain(ctx, "#2").Reasons).To(HaveExactElements(BeReason(domain.ReasonBlocked, 1)))
	})

	It("Releasing a hold returns the work", func(ctx SpecContext) {
		By("given a held node")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.hold(ctx, tx, "#1", "parked")
		})

		By("when the hold is released")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			_, err := tx.UnholdNode(ctx, store.UnholdNodeParams{OrgID: g.org, ID: g.id("#1")})
			Expect(err).NotTo(HaveOccurred())
		})

		By("then it is ready again")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1)))
		Expect(g.explain(ctx, "#1").Ready()).To(BeTrue())
	})
})

var _ = Describe("Focus", Label(testsupport.DBLabel), func() {
	var (
		s   *store.Store
		g   *graph
		svc *domain.Service
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		svc = domain.NewService(s.Pool(), pol)
	})

	It("A sweep parks everything outside the keep-set and leaves the lineage of what it keeps", func(ctx SpecContext) {
		By("given epic 1 with child 2, and unrelated nodes 3 and 4")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			one := g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, &one)
			g.node(ctx, tx, 3, nil)
			g.node(ctx, tx, 4, nil)
		})

		By("when I focus the project on node 2")
		result, err := svc.Focus(ctx, g.org, user, "focus-1", "platform", []string{"#2"}, "this week")
		Expect(err).NotTo(HaveOccurred())

		By("then 3 and 4 are parked and the kept lineage is not")
		Expect(result.Held).To(Equal(2))
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(2)))
		Expect(g.explain(ctx, "#1").Reasons).To(HaveExactElements(BeReason(domain.ReasonOpenChild, 2)))
		Expect(g.explain(ctx, "#3").Reasons).To(HaveExactElements(BeReason(domain.ReasonHeld, 3)))
	})

	It("Focusing again replaces the sweep rather than stacking one", func(ctx SpecContext) {
		By("given three sibling nodes and a sweep keeping node 1")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.node(ctx, tx, 3, nil)
		})
		first, err := svc.Focus(ctx, g.org, user, "focus-1", "platform", []string{"#1"}, "this week")
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Held).To(Equal(2))

		By("when I focus on node 2 instead")
		second, err := svc.Focus(ctx, g.org, user, "focus-2", "platform", []string{"#2"}, "next week")
		Expect(err).NotTo(HaveOccurred())

		By("then the first sweep was released and a new one placed")
		Expect(second.Released).To(Equal(2))
		Expect(second.Group).NotTo(Equal(first.Group))
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(2)))

		By("and clearing it needs no sweep id")
		released, err := svc.ClearFocus(ctx, g.org, user, "clear-1", "platform", "", "week over")
		Expect(err).NotTo(HaveOccurred())
		Expect(released).To(Equal(2))
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1), int64(2), int64(3)))
	})

	It("A sweep neither adopts nor releases a hold placed on its own", func(ctx SpecContext) {
		By("given node 1 parked by hand, and node 2 open")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.node(ctx, tx, 3, nil)
		})
		_, err := g.apply(ctx, user, "hold-1", domain.Op{
			HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked in march"},
		})
		Expect(err).NotTo(HaveOccurred())

		By("when a sweep keeps node 2 and is then cleared")
		result, err := svc.Focus(ctx, g.org, user, "focus-1", "platform", []string{"#2"}, "this week")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Held).To(Equal(1), "only node 3; the hold on node 1 is not the sweep's to take")
		_, err = svc.ClearFocus(ctx, g.org, user, "clear-1", "platform", "", "week over")
		Expect(err).NotTo(HaveOccurred())

		By("then the hand-placed hold outlives it")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(2), int64(3)))
		Expect(g.explain(ctx, "#1").Reasons).To(HaveExactElements(BeReason(domain.ReasonHeld, 1)))
		Expect(g.explain(ctx, "#1").Reasons[0].Hold.Reason).To(Equal("parked in march"))
	})

	It("An agent may not park work, and the refusal names the next move", func(ctx SpecContext) {
		By("given an open node and an agent principal")
		agent := g.agent(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
		})

		By("when the agent holds it")
		_, err := g.apply(ctx, agent, "hold-1", domain.Op{
			HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked"},
		})

		By("then it is refused, and told to comment instead")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no rule permits agent hold_node"))
		Expect(err.Error()).To(ContainSubstring("comment"))
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1)))
	})

	It("A held node is never offered to a claim", func(ctx SpecContext) {
		By("given the project's only open node, parked")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
		})
		_, err := g.apply(ctx, user, "hold-1", domain.Op{
			HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked"},
		})
		Expect(err).NotTo(HaveOccurred())

		By("when an agent claims from the project")
		agent := g.agent(ctx)
		claimed, err := domain.Claim(ctx, s, pol, g.org, agent, domain.ClaimRequest{
			RequestID: "claim-1", Projects: []string{"platform"}, Harness: "spec", AgentRef: "spec",
		})

		By("then there is nothing to claim: a hold is not a claimable state")
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed).To(BeNil())
	})

	It("A hold carries a reason, and holding a held node is a conflict", func(ctx SpecContext) {
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
		})

		By("when a hold carries no reason")
		_, err := g.apply(ctx, user, "hold-0", domain.Op{HoldNode: &domain.HoldNode{Node: "#1"}})
		Expect(err).To(MatchError(domain.ErrReasonRequired))

		By("and when the same node is held twice")
		_, err = g.apply(ctx, user, "hold-1", domain.Op{HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked"}})
		Expect(err).NotTo(HaveOccurred())
		_, err = g.apply(ctx, user, "hold-2", domain.Op{HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked"}})
		Expect(err).To(MatchError(domain.ErrStatusConflict))

		By("and when a node with no hold is released")
		_, err = g.apply(ctx, user, "unhold-1", domain.Op{UnholdNode: &domain.UnholdNode{Node: "#1", Reason: "back"}})
		Expect(err).NotTo(HaveOccurred())
		_, err = g.apply(ctx, user, "unhold-2", domain.Op{UnholdNode: &domain.UnholdNode{Node: "#1", Reason: "back"}})
		Expect(err).To(MatchError(domain.ErrNotHeld))
	})
})
