package domain_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// A sweep's events carry the row as it stood: a hold's before image is
// the unheld row, and a release's before image names the sweep that
// held it.
var _ = Describe("Sweep events", Label(testsupport.DBLabel), func() {
	var (
		s    *store.Store
		g    *graph
		svc  *domain.Service
		user domain.Actor
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		svc = domain.NewService(s.Pool(), pol)
		user = g.principal(ctx)
	})

	It("record a node's hold from the unheld row, and its release from the sweep that held it", func(ctx SpecContext) {
		By("given a project focused on one of its three nodes")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.node(ctx, tx, 3, nil)
		})
		focused, err := svc.Focus(ctx, g.org, user, "focus-1", "platform", []string{"#1"}, "this week")
		Expect(err).NotTo(HaveOccurred())
		Expect(focused.Held).To(Equal(2))

		By("then each hold event goes from a node held by nothing to one held by the sweep")
		holds := g.eventsFor(ctx, "focus-1")
		Expect(holds).To(HaveLen(2))
		for _, e := range holds {
			Expect(e.Op).To(Equal(domain.OpHoldNode))
			Expect(field(e.Before, "HeldAt")).To(BeNil())
			Expect(field(e.Before, "HoldGroup")).To(BeNil())
			Expect(field(e.After, "HoldGroup")).To(Equal(focused.Group))
		}

		By("and when the focus is cleared, each release event goes from the sweep's hold to none")
		released, err := svc.ClearFocus(ctx, g.org, user, "clear-1", "platform", "", "week over")
		Expect(err).NotTo(HaveOccurred())
		Expect(released).To(Equal(2))
		releases := g.eventsFor(ctx, "clear-1")
		Expect(releases).To(HaveLen(2))
		for _, e := range releases {
			Expect(e.Op).To(Equal(domain.OpUnholdNode))
			Expect(field(e.Before, "HoldGroup")).To(Equal(focused.Group))
			Expect(field(e.After, "HoldGroup")).To(BeNil())
			Expect(field(e.After, "HeldAt")).To(BeNil())
		}
	})

	It("record a project's hold and release the same way", func(ctx SpecContext) {
		By("given an organization focused on one of its two projects")
		other := g.newProject(ctx, "other")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.nodeIn(ctx, tx, other, "other", 1, nil)
		})
		focused, err := svc.Focus(ctx, g.org, user, "focus-org", "", []string{"platform"}, "platform week")
		Expect(err).NotTo(HaveOccurred())
		Expect(focused.Held).To(Equal(1))

		By("then the hold event goes from a project held by nothing to one held by the sweep")
		holds := g.eventsFor(ctx, "focus-org")
		Expect(holds).To(HaveLen(1))
		Expect(holds[0].Op).To(Equal(domain.OpHoldProject))
		Expect(field(holds[0].Before, "HeldAt")).To(BeNil())
		Expect(field(holds[0].Before, "HoldGroup")).To(BeNil())
		Expect(field(holds[0].After, "HoldGroup")).To(Equal(focused.Group))

		By("and when the focus is cleared, the release event goes from the sweep's hold to none")
		released, err := svc.ClearFocus(ctx, g.org, user, "clear-org", "", "", "week over")
		Expect(err).NotTo(HaveOccurred())
		Expect(released).To(Equal(1))
		releases := g.eventsFor(ctx, "clear-org")
		Expect(releases).To(HaveLen(1))
		Expect(releases[0].Op).To(Equal(domain.OpUnholdProject))
		Expect(field(releases[0].Before, "HoldGroup")).To(Equal(focused.Group))
		Expect(field(releases[0].After, "HoldGroup")).To(BeNil())
	})
})
