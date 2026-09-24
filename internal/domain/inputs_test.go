package domain_test

import (
	"context"
	"fmt"
	"testing/fstest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// readInputs reads the inputs of a node under bundle, in a transaction of
// its own.
func (g *graph) readInputs(ctx context.Context, bundle *policy.Bundle, actor domain.Actor, ref string) ([]domain.InputState, error) {
	var in []domain.InputState
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		in, err = domain.ReadInputs(ctx, tx, bundle, g.org, actor, "platform", ref)
		return err
	})
	return in, err
}

// inputsSize reads the hint for a node in a transaction of its own.
func (g *graph) inputsSize(ctx context.Context, ref string) domain.InputsHint {
	GinkgoHelper()
	var hint domain.InputsHint
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		hint, err = domain.InputsSize(ctx, tx, g.org, g.id(ref))
		return err
	})).To(Succeed())
	return hint
}

var _ = Describe("Reading the inputs of a node", Label(testsupport.DBLabel), func() {
	var (
		s     *store.Store
		g     *graph
		user  domain.Actor
		agent domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		user = g.principal(ctx)
		agent = g.agent(ctx)

		By("given nodes 1 and 2 done with results, 3 open and 4 dropped, all blocking node 5")
		g.mustApply(ctx, user, "setup",
			create("a", "Alpha"), create("b", "Beta"), create("d", "Delta"), create("e", "Epsilon"),
			domain.Op{CreateNode: &domain.CreateNode{Alias: "c", Title: "Gamma", BlockedBy: []string{"a", "b", "d", "e"}}},
		)
		g.mustApply(ctx, user, "results", complete("#1", "alpha"), complete("#2", "beta"))
		g.mustApply(ctx, user, "dropped", drop("#4", domain.DropSuperseded))
	})

	It("returns every predecessor that satisfies the node, and no open one", func(ctx SpecContext) {
		in, err := g.readInputs(ctx, pol, agent, "#5")
		Expect(err).NotTo(HaveOccurred())
		Expect(in).To(HaveLen(3))
		Expect(in[0].Ref).To(Equal("platform#1"))
		Expect(in[0].Result.Body).To(Equal("alpha"))
		Expect(in[1].Ref).To(Equal("platform#2"))
		Expect(in[1].Result.Body).To(Equal("beta"))
		Expect(in[2].Ref).To(Equal("platform#4"))
		Expect(in[2].Status).To(Equal("dropped"))
		Expect(in[2].Result).To(BeNil())
	})

	It("sizes the results a node's satisfied predecessors carry", func(ctx SpecContext) {
		Expect(g.inputsSize(ctx, "#5")).To(Equal(domain.InputsHint{Count: 2, Bytes: int64(len("alpha") + len("beta"))}))
		Expect(g.inputsSize(ctx, "#1")).To(Equal(domain.InputsHint{}))
	})

	It("reports an unknown node", func(ctx SpecContext) {
		_, err := g.readInputs(ctx, pol, agent, "#99")
		Expect(err).To(MatchError(domain.ErrNodeNotFound))
	})

	Describe("Authorization", func() {
		It("lets an agent principal with read on the project call it, and rejects one without", func(ctx SpecContext) {
			By("given a policy that lets a principal read the inputs of one project only")
			elsewhere := g.newProject(ctx, "elsewhere")
			narrow, err := policy.Load(ctx, fstest.MapFS{"narrow.rego": &fstest.MapFile{Data: []byte(fmt.Sprintf(`
package skift.authz

default allow := false

allow if {
	input.action == "read_inputs"
	input.resource.node.project_id == %q
}

decision := {"allow": allow, "reasons": []}
`, elsewhere))}})
			Expect(err).NotTo(HaveOccurred())

			By("when an agent reads the inputs of a node outside it")
			_, err = g.readInputs(ctx, narrow, agent, "#5")

			By("then the read is refused")
			Expect(err).To(MatchError(policy.ErrDenied))
			var denial *domain.Denial
			Expect(err).To(BeAssignableToTypeOf(denial))

			By("and the same agent may read under the default bundle, which grants the organization the read")
			in, err := g.readInputs(ctx, pol, agent, "#5")
			Expect(err).NotTo(HaveOccurred())
			Expect(in).To(HaveLen(3))
		})

		It("is closed when no policy could be loaded", func(ctx SpecContext) {
			var none *policy.Bundle
			_, err := g.readInputs(ctx, none, agent, "#5")
			Expect(err).To(MatchError(policy.ErrDenied))
		})
	})
})
