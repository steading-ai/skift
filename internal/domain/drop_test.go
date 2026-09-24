package domain_test

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// cascadingDrop is a drop_node operation on node #1, taking the subtree it
// blocks within the bound it states.
func cascadingDrop(reason string, maxNodes int) domain.Op {
	return domain.Op{DropNode: &domain.DropNode{Node: "#1", Reason: reason, Cascade: &domain.Cascade{MaxNodes: maxNodes}}}
}

// chain lays out a blocking chain of n nodes, node 1 blocking node 2 and
// so on, and returns their references in order.
func (g *graph) chain(ctx context.Context, actor domain.Actor, n int) []string {
	GinkgoHelper()
	ops := make([]domain.Op, 0, 2*n)
	refs := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		alias := fmt.Sprintf("n%d", i)
		ops = append(ops, create(alias, fmt.Sprintf("Node %d", i)))
		if i > 1 {
			ops = append(ops, addEdge(fmt.Sprintf("n%d", i-1), alias))
		}
		refs = append(refs, fmt.Sprintf("platform#%d", i))
	}
	g.mustApply(ctx, actor, "chain", ops...)
	return refs
}

var _ = Describe("Dropping nodes", Label(testsupport.DBLabel), func() {
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
		It("A cascade must state a bound it fits within", func(ctx SpecContext) {
			By("given #1 blocks #2 which blocks #3 and #4")
			g.mustApply(ctx, actor, "layout",
				create("one", "One"), create("two", "Two"), create("three", "Three"), create("four", "Four"),
				addEdge("one", "two"), addEdge("two", "three"), addEdge("two", "four"))

			By("when #1 is dropped with cascade max_nodes 2")
			_, err := g.apply(ctx, actor, "axe", cascadingDrop(domain.DropSuperseded, 2))

			By("then the patch is rejected naming 4 affected nodes against a bound of 2")
			var refusal *domain.CascadeTooLargeError
			Expect(errors.As(err, &refusal)).To(BeTrue(), "%v", err)
			Expect(refusal.Count).To(Equal(4))
			Expect(refusal.MaxNodes).To(Equal(2))
			Expect(err.Error()).To(And(ContainSubstring("4"), ContainSubstring("2")))

			By("and no node's status changed")
			Expect(g.statuses(ctx, "#1", "#2", "#3", "#4")).To(Equal([]string{"open", "open", "open", "open"}))
			Expect(g.countEvents(ctx, "axe")).To(BeZero())
		})

		It("A cascade within its bound returns the undo list", func(ctx SpecContext) {
			By("given the same graph")
			g.mustApply(ctx, actor, "layout",
				create("one", "One"), create("two", "Two"), create("three", "Three"), create("four", "Four"),
				addEdge("one", "two"), addEdge("two", "three"), addEdge("two", "four"))

			By("when #1 is dropped with cascade max_nodes 4")
			r, err := g.apply(ctx, actor, "axe", cascadingDrop(domain.DropSuperseded, 4))
			Expect(err).NotTo(HaveOccurred())

			By("then #1, #2, #3 and #4 are dropped")
			Expect(g.statuses(ctx, "#1", "#2", "#3", "#4")).To(Equal([]string{"dropped", "dropped", "dropped", "dropped"}))

			By("and the response lists exactly those four references")
			Expect(r.Dropped).To(ConsistOf("platform#1", "platform#2", "platform#3", "platform#4"))
		})

		It("A cascade that leaves the project names the other project's node by its own slug", func(ctx SpecContext) {
			By("given #1 blocking a node of another project")
			spike := g.newProject(ctx, "spike")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.nodeIn(ctx, tx, spike, "spike", 1, nil)
				g.edge(ctx, tx, "platform#1", "spike#1")
			})

			By("when #1 is dropped with cascade")
			r, err := g.apply(ctx, actor, "axe", cascadingDrop(domain.DropSuperseded, 4))
			Expect(err).NotTo(HaveOccurred())

			By("then the undo list names each node under its own project")
			Expect(r.Dropped).To(ConsistOf("platform#1", "spike#1"))
			Expect(g.getNode(ctx, "spike#1").Status).To(Equal("dropped"))
		})

		It("A cascade cannot clear a gate", func(ctx SpecContext) {
			By("given #1 blocks gate #2 which blocks #3")
			// The gate is filed by someone else: the policy stops the
			// principal that filed a gate from disposing of it, and the
			// question here is the cascade, not that veto.
			filer := g.principalOf(ctx, "user")
			g.mustApply(ctx, filer, "layout",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", AssigneeKind: domain.AssigneeUser}},
				create("three", "Three"),
				addEdge("one", "two"), addEdge("two", "three"))

			By("when #1 is dropped with cascade max_nodes 10")
			_, err := g.apply(ctx, actor, "axe", cascadingDrop(domain.DropSuperseded, 10))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.statuses(ctx, "#1", "#2", "#3")).To(Equal([]string{"dropped", "dropped", "dropped"}))

			By("then #3 remains blocked by #2")
			e := g.explain(ctx, "#3")
			Expect(e.Ready()).To(BeFalse())
			Expect(e.Reasons).To(ContainElement(BeReason(domain.ReasonBlocked, 2)))
		})
	})

	Describe("The stated bound", func() {
		It("refuses a subtree of seven nodes against a bound of five, naming both", func(ctx SpecContext) {
			refs := g.chain(ctx, actor, 7)

			_, err := g.apply(ctx, actor, "axe", cascadingDrop(domain.DropNotViable, 5))
			var refusal *domain.CascadeTooLargeError
			Expect(errors.As(err, &refusal)).To(BeTrue(), "%v", err)
			Expect(refusal.Count).To(Equal(7))
			Expect(refusal.MaxNodes).To(Equal(5))
			Expect(err.Error()).To(And(ContainSubstring("7"), ContainSubstring("5")))
			Expect(g.statuses(ctx, refs...)).To(HaveEach("open"))
		})

		It("drops the same seven and lists their seven references under a bound of seven", func(ctx SpecContext) {
			refs := g.chain(ctx, actor, 7)

			r, err := g.apply(ctx, actor, "axe", cascadingDrop(domain.DropNotViable, 7))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.statuses(ctx, refs...)).To(HaveEach("dropped"))
			Expect(r.Dropped).To(ConsistOf("platform#1", "platform#2", "platform#3", "platform#4", "platform#5", "platform#6", "platform#7"))
		})

		It("counts the node the drop names, so a cascade of one node needs a bound of one", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "layout", create("one", "One"))

			_, err := g.apply(ctx, actor, "too-small", cascadingDrop(domain.DropSuperseded, 0))
			Expect(err).To(MatchError(domain.ErrCascadeBoundRequired))

			r, err := g.apply(ctx, actor, "just-right", cascadingDrop(domain.DropSuperseded, 1))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Dropped).To(Equal([]string{"platform#1"}))
		})
	})

	Describe("The dropped set", func() {
		It("lists the one reference a drop without cascade takes", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "layout",
				create("one", "One"), create("two", "Two"), addEdge("one", "two"))

			r, err := g.apply(ctx, actor, "axe", drop("#1", domain.DropNoLongerRelevant))
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Dropped).To(Equal([]string{"platform#1"}))
			Expect(g.statuses(ctx, "#1", "#2")).To(Equal([]string{"dropped", "open"}))
		})

		It("is empty for a patch that drops nothing", func(ctx SpecContext) {
			r := g.mustApply(ctx, actor, "layout", create("one", "One"))
			Expect(r.Dropped).To(BeEmpty())
		})
	})
})
