package domain_test

import (
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("The claim cursor", Label(testsupport.DBLabel), func() {
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

	It("belongs to the filter that made it, so a different filter starts its own rotation", func(ctx SpecContext) {
		By("given two projects with two ready nodes each")
		g.mustApply(ctx, actor, "first", create("a1", "A1"), create("a2", "A2"))
		second := g.newProject(ctx, "second")
		_, err := g.applyPatch(ctx, actor, domain.Patch{RequestID: "second", Project: "second", Ops: []domain.Op{create("b1", "B1"), create("b2", "B2")}})
		Expect(err).NotTo(HaveOccurred())
		// The rotation visits projects in id order, and the ids are random.
		projects := []string{g.project, second}
		sort.Strings(projects)
		first, next := projects[0], projects[1]
		agent := g.agent(ctx)
		everywhere := domain.ClaimRequest{RequestID: "all-1", TTL: time.Minute, Harness: "spec", AgentRef: "spec"}
		tasks := domain.ClaimRequest{RequestID: "tasks-1", Kinds: []string{"task"}, TTL: time.Minute, Harness: "spec", AgentRef: "spec"}

		By("when the agent claims across every project, its rotation starts at the first")
		c := g.mustClaim(ctx, agent, everywhere)
		Expect(c.Node.ProjectID).To(Equal(first))

		By("then a claim filtered by kind, whose ready set is the same, starts a rotation of its own at the first project too")
		c = g.mustClaim(ctx, agent, tasks)
		Expect(c.Node.ProjectID).To(Equal(first), "the kind filter has no cursor yet, so it does not take the unfiltered claim's turn")

		By("and the unfiltered rotation moves on to the next project, untouched by the filtered claim")
		everywhere.RequestID = "all-2"
		c = g.mustClaim(ctx, agent, everywhere)
		Expect(c.Node.ProjectID).To(Equal(next))
	})
})
