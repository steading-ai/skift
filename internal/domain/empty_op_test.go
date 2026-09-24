package domain_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Patch validation", Label(testsupport.DBLabel), func() {
	It("An operation that sets nothing is the request's mistake, and the patch changes nothing", func(ctx SpecContext) {
		s := store.FromPool(pg.Database(ctx))
		g := newGraph(ctx, s)
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })

		_, err := g.apply(ctx, user, "empty-1",
			domain.Op{HoldNode: &domain.HoldNode{Node: "#1", Reason: "parked"}},
			domain.Op{},
		)
		Expect(err).To(MatchError(domain.ErrEmptyOperation))
		Expect(g.explain(ctx, "#1").Reasons).To(BeEmpty(), "the hold before it did not commit")
		Expect(g.eventsFor(ctx, "empty-1")).To(BeEmpty())
	})

	It("A patch names its project by slug, id, or org/slug, and an alias once", func(ctx SpecContext) {
		s := store.FromPool(pg.Database(ctx))
		g := newGraph(ctx, s)
		user := g.principal(ctx)
		create := func(alias string) domain.Op {
			return domain.Op{CreateNode: &domain.CreateNode{Alias: alias, Title: "Node " + alias}}
		}

		By("when the project qualifier names an organization that is not the caller's")
		_, err := g.applyPatch(ctx, user, domain.Patch{RequestID: "p-1", Project: "nowhere/platform", Ops: []domain.Op{create("a")}})
		var missing *domain.NotFoundError
		Expect(errors.As(err, &missing)).To(BeTrue(), "got %v", err)
		Expect(missing.Missing).To(Equal(domain.MissingOrganization))

		By("and when an id-shaped reference names no project, it is read as a slug and not found")
		_, err = g.applyPatch(ctx, user, domain.Patch{RequestID: "p-2", Project: "01JNOSUCHPROJECT0000000000", Ops: []domain.Op{create("a")}})
		Expect(err).To(MatchError(domain.ErrProjectNotFound))

		By("and when an alias is bound twice, or is not an alias")
		_, err = g.applyPatch(ctx, user, domain.Patch{RequestID: "p-3", Project: g.project, Ops: []domain.Op{create("a"), create("a")}})
		Expect(err).To(MatchError(ContainSubstring(`alias "a" is bound twice`)))
		_, err = g.applyPatch(ctx, user, domain.Patch{RequestID: "p-4", Project: g.project, Ops: []domain.Op{create("a#1")}})
		Expect(err).To(MatchError(ContainSubstring("an alias may not contain #")))
		Expect(g.countNodes(ctx)).To(BeZero(), "no refused patch created anything")

		By("and an assignee is carried on creation and on update")
		agent := g.agent(ctx)
		res := g.mustApply(ctx, user, "p-5", domain.Op{CreateNode: &domain.CreateNode{
			Alias: "a", Title: "Assigned", AssigneeKind: domain.AssigneeAgent, Assignee: agent.PrincipalID,
		}})
		Expect(res.Nodes).To(HaveLen(1))
		Expect(g.getNode(ctx, "#1").Assignee).To(HaveValue(Equal(agent.PrincipalID)))
		g.mustApply(ctx, user, "p-6", domain.Op{UpdateNode: &domain.UpdateNode{Node: "#1", Assignee: ptr("")}})
		Expect(g.getNode(ctx, "#1").Assignee).To(BeNil())
	})
})
