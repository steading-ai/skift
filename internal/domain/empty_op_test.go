package domain_test

import (
	"context"

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
})
