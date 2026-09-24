package domain_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// stateNumbers renders annotated nodes as their numbers, in order.
func stateNumbers(states []domain.NodeState) []int64 {
	out := make([]int64, 0, len(states))
	for _, s := range states {
		out = append(out, s.Number)
	}
	return out
}

var _ = Describe("Project service", Label(testsupport.DBLabel), func() {
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

	It("creates a project under its request id, deriving or normalizing the slug and keeping it unique", func(ctx SpecContext) {
		By("when the request is missing what a project owes")
		_, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{Title: "Billing"})
		Expect(err).To(MatchError(domain.ErrNoRequestID))
		_, err = svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-0", Title: "   "})
		Expect(err).To(MatchError(domain.ErrTitleRequired))
		_, err = svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-0", Title: "Billing", Priority: "p9"})
		Expect(err).To(MatchError(ContainSubstring("p0 through p4")))
		_, err = svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-0", Title: "Billing", VCSURL: "not a url"})
		Expect(err).To(MatchError(domain.ErrVCSURLInvalid))
		Expect(g.eventsFor(ctx, "p-0")).To(BeEmpty())

		By("when a title is given, the slug is derived from it")
		billing, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-1", Title: "Billing Service", VCSURL: "https://example.com/billing.git"})
		Expect(err).NotTo(HaveOccurred())
		Expect(billing.Slug).To(Equal("billing-service"))
		Expect(billing.Priority).To(Equal("p2"), "the default")
		Expect(billing.VcsUrl).To(Equal("https://example.com/billing.git"))
		events := g.eventsFor(ctx, "p-1")
		Expect(events).To(HaveLen(1))
		Expect(events[0].Op).To(Equal(domain.OpCreateProject))
		Expect(events[0].ProjectID).To(HaveValue(Equal(billing.ID)))

		By("and a requested slug is normalized rather than derived")
		custom, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-2", Title: "Anything", Slug: "Billing Ops", Priority: "p0"})
		Expect(err).NotTo(HaveOccurred())
		Expect(custom.Slug).To(Equal("billing-ops"))
		Expect(custom.Priority).To(Equal("p0"))

		By("and a slug already taken gets a numeric suffix")
		again, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-3", Title: "Billing Service"})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Slug).To(MatchRegexp(`^billing-service-\d+$`))

		By("and a suffix on a slug at the schema's full length makes room for itself")
		long := strings.Repeat("x", 120)
		first, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-long-1", Title: long})
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Slug).To(HaveLen(100))
		next, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-long-2", Title: long})
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Slug).To(HaveLen(100))
		Expect(next.Slug).To(HaveSuffix("-2"))

		By("and repeating a request replays it rather than creating twice")
		replayed, err := svc.CreateProject(ctx, g.org, user, domain.NewProject{RequestID: "p-1", Title: "Billing Service", VCSURL: "https://example.com/billing.git"})
		Expect(err).NotTo(HaveOccurred())
		Expect(replayed.ID).To(Equal(billing.ID))
		Expect(g.eventsFor(ctx, "p-1")).To(HaveLen(1))
	})

	It("updates a project's repository, recording only a change", func(ctx SpecContext) {
		By("when the request is missing what it owes")
		_, err := svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{Project: "platform"})
		Expect(err).To(MatchError(domain.ErrNoRequestID))
		_, err = svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-0"})
		Expect(err).To(MatchError(domain.ErrProjectRequired))
		bad := "nowhere"
		_, err = svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-0", Project: "platform", VCSURL: &bad})
		Expect(err).To(MatchError(domain.ErrVCSURLInvalid))
		_, err = svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-0", Project: "nowhere"})
		Expect(err).To(MatchError(domain.ErrProjectNotFound))

		By("when the repository is set")
		url := "https://example.com/platform.git"
		updated, err := svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-1", Project: "platform", VCSURL: &url})
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.VcsUrl).To(Equal(url))
		events := g.eventsFor(ctx, "u-1")
		Expect(events).To(HaveLen(1))
		Expect(events[0].Op).To(Equal(domain.OpUpdateProject))

		By("then setting it to the same value, or leaving it unset, records nothing")
		same, err := svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-2", Project: "platform", VCSURL: &url})
		Expect(err).NotTo(HaveOccurred())
		Expect(same.VcsUrl).To(Equal(url))
		Expect(g.eventsFor(ctx, "u-2")).To(BeEmpty())
		untouched, err := svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-3", Project: "platform"})
		Expect(err).NotTo(HaveOccurred())
		Expect(untouched.VcsUrl).To(Equal(url))
		Expect(g.eventsFor(ctx, "u-3")).To(BeEmpty())

		By("and clearing it is a change")
		empty := ""
		cleared, err := svc.UpdateProject(ctx, g.org, user, domain.UpdateProject{RequestID: "u-4", Project: "platform", VCSURL: &empty})
		Expect(err).NotTo(HaveOccurred())
		Expect(cleared.VcsUrl).To(BeEmpty())
		Expect(g.eventsFor(ctx, "u-4")).To(HaveLen(1))
	})

	It("archives a project once, and lists it only when asked", func(ctx SpecContext) {
		_, err := svc.ArchiveProject(ctx, g.org, user, "", "platform")
		Expect(err).To(MatchError(domain.ErrNoRequestID))
		_, err = svc.ArchiveProject(ctx, g.org, user, "a-0", "")
		Expect(err).To(MatchError(domain.ErrProjectRequired))
		_, err = svc.GetProject(ctx, g.org, "")
		Expect(err).To(MatchError(domain.ErrProjectRequired))

		archived, err := svc.ArchiveProject(ctx, g.org, user, "a-1", "platform")
		Expect(err).NotTo(HaveOccurred())
		Expect(archived.ArchivedAt).NotTo(BeNil())
		events := g.eventsFor(ctx, "a-1")
		Expect(events).To(HaveLen(1))
		Expect(events[0].Op).To(Equal(domain.OpArchiveProject))

		again, err := svc.ArchiveProject(ctx, g.org, user, "a-2", "platform")
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ArchivedAt).To(Equal(archived.ArchivedAt))
		Expect(g.eventsFor(ctx, "a-2")).To(BeEmpty())

		live, err := svc.ListProjects(ctx, g.org, domain.ProjectListQuery{})
		Expect(err).NotTo(HaveOccurred())
		Expect(live).To(BeEmpty())
		all, err := svc.ListProjects(ctx, g.org, domain.ProjectListQuery{IncludeArchived: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
		Expect(all[0].Slug).To(Equal("platform"))

		_, err = svc.ListProjects(ctx, g.org, domain.ProjectListQuery{Offset: -1})
		Expect(err).To(MatchError("offset is negative"))
		_, err = svc.ListProjects(ctx, g.org, domain.ProjectListQuery{Limit: domain.ActiveLimits().PageSize + 1})
		Expect(err).To(HaveOccurred(), "a page over the ceiling is refused, not clamped")
	})

	Describe("ListNodes", func() {
		It("narrows by status, kind, labels, priority, hold, and parent, and refuses a filter it cannot read", func(ctx SpecContext) {
			By("given an epic with a spike and a task under it, and a done sibling")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &one)
				g.node(ctx, tx, 3, &one)
				g.node(ctx, tx, 4, nil)
				g.setStatus(ctx, tx, "#4", "done")
			})
			g.mustApply(ctx, user, "shape",
				domain.Op{UpdateNode: &domain.UpdateNode{Node: "#2", Kind: ptr("spike"), Labels: &[]string{"research", "auth"}}},
				domain.Op{UpdateNode: &domain.UpdateNode{Node: "#3", Priority: ptr("p0"), Labels: &[]string{"auth"}}},
				domain.Op{HoldNode: &domain.HoldNode{Node: "#3", Reason: "parked"}},
			)

			list := func(q domain.NodeListQuery) []int64 {
				GinkgoHelper()
				states, err := svc.ListNodes(ctx, g.org, q)
				Expect(err).NotTo(HaveOccurred())
				return stateNumbers(states)
			}

			By("then each filter narrows the page and never widens it")
			Expect(list(domain.NodeListQuery{})).To(Equal([]int64{1, 2, 3, 4}))
			Expect(list(domain.NodeListQuery{Status: "done"})).To(Equal([]int64{4}))
			Expect(list(domain.NodeListQuery{Kinds: []string{"spike"}})).To(Equal([]int64{2}))
			Expect(list(domain.NodeListQuery{Labels: []string{"research", "auth"}})).To(Equal([]int64{2}))
			Expect(list(domain.NodeListQuery{AnyLabels: []string{"research", "auth"}})).To(Equal([]int64{2, 3}))
			Expect(list(domain.NodeListQuery{MinPriority: "p1"})).To(Equal([]int64{3}))
			Expect(list(domain.NodeListQuery{Held: true})).To(Equal([]int64{3}))
			Expect(list(domain.NodeListQuery{Projects: []string{"platform"}, Parent: "#1"})).To(Equal([]int64{2, 3}))
			Expect(list(domain.NodeListQuery{Parent: "platform#1"})).To(Equal([]int64{2, 3}), "a qualified parent needs no project")
			Expect(list(domain.NodeListQuery{Limit: 2, Offset: 1})).To(Equal([]int64{2, 3}))

			By("and a project hold parks everything open in it")
			_, _, err := svc.HoldProject(ctx, g.org, user, "hold-p", "platform", "not this quarter")
			Expect(err).NotTo(HaveOccurred())
			Expect(list(domain.NodeListQuery{Held: true})).To(Equal([]int64{1, 2, 3, 4}), "the hold filter is about the hold; status is its own filter")
			Expect(list(domain.NodeListQuery{Held: true, Status: "open"})).To(Equal([]int64{1, 2, 3}))

			By("and a filter that names nothing the schema knows is refused")
			_, err = svc.ListNodes(ctx, g.org, domain.NodeListQuery{Status: "weird"})
			Expect(err).To(MatchError(ContainSubstring("not open, done, or dropped")))
			_, err = svc.ListNodes(ctx, g.org, domain.NodeListQuery{AssigneeKind: "robot"})
			Expect(err).To(MatchError(ContainSubstring("not agent or user")))
			_, err = svc.ListNodes(ctx, g.org, domain.NodeListQuery{MinPriority: "p9"})
			Expect(err).To(MatchError(ContainSubstring("p0 through p4")))
			_, err = svc.ListNodes(ctx, g.org, domain.NodeListQuery{Offset: -1})
			Expect(err).To(MatchError("offset is negative"))
			_, err = svc.ListNodes(ctx, g.org, domain.NodeListQuery{Parent: "#1"})
			Expect(err).To(MatchError(domain.ErrNoDefaultProject), "a bare number needs a project to resolve in")
			_, err = svc.ListNodes(ctx, g.org, domain.NodeListQuery{Projects: []string{"nowhere"}})
			Expect(err).To(MatchError(domain.ErrProjectNotFound))
		})
	})

	Describe("Ready under a parent", func() {
		It("lists the ready children of one node, resolving the parent in the query's project or its own", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &one)
				g.node(ctx, tx, 3, &one)
				g.node(ctx, tx, 4, nil)
			})
			Expect(numbers(g.ready(ctx, domain.ReadyQuery{Parent: "#1"}))).To(ConsistOf(int64(2), int64(3)))
			set, err := g.readySet(ctx, domain.ReadyQuery{Parent: "platform#1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(numbers(set)).To(ConsistOf(int64(2), int64(3)))

			_, err = g.readySet(ctx, domain.ReadyQuery{Parent: "#1"})
			Expect(err).To(MatchError(domain.ErrNoDefaultProject))
			_, err = g.readySet(ctx, domain.ReadyQuery{Projects: []string{g.project}, Parent: "#9"})
			Expect(err).To(MatchError(domain.ErrNodeNotFound))

			By("and pages the ready set by limit and offset, in the order it lists")
			all, err := g.readySet(ctx, domain.ReadyQuery{Projects: []string{g.project}})
			Expect(err).NotTo(HaveOccurred())
			Expect(numbers(all)).To(ConsistOf(int64(2), int64(3), int64(4)))
			page, err := g.readySet(ctx, domain.ReadyQuery{Projects: []string{g.project}, Limit: 2, Offset: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(numbers(page)).To(Equal(numbers(all)[1:3]), "the page skips the first of the three")

			Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := domain.Ready(ctx, tx, g.org, domain.ReadyQuery{})
				Expect(err).To(MatchError(domain.ErrOneProject))
				_, err = domain.Ready(ctx, tx, g.org, domain.ReadyQuery{Projects: []string{g.project}, Offset: -1})
				Expect(err).To(MatchError("offset is negative"))
				return nil
			})).To(Succeed())
		})
	})
})
