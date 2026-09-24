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

// projectIDs renders nodes as the projects they belong to, deduplicated.
func projectIDs(nodes []store.Node) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range nodes {
		if !seen[n.ProjectID] {
			seen[n.ProjectID] = true
			out = append(out, n.ProjectID)
		}
	}
	return out
}

var _ = Describe("Project holds", Label(testsupport.DBLabel), func() {
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

	It("Holding a project parks its open work, and the decision is recorded once", func(ctx SpecContext) {
		By("given two open nodes and one done node")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.node(ctx, tx, 3, nil)
			g.setStatus(ctx, tx, "#3", "done")
		})

		By("when a user holds the project")
		project, held, err := svc.HoldProject(ctx, g.org, user, "hold-p-1", "platform", "not this quarter")
		Expect(err).NotTo(HaveOccurred())

		By("then the project carries the hold, by hand rather than by a sweep")
		Expect(project.HeldAt).NotTo(BeNil())
		Expect(project.HeldBy).To(HaveValue(Equal(user.PrincipalID)))
		Expect(project.HeldReason).To(HaveValue(Equal("not this quarter")))
		Expect(project.HoldGroup).To(BeNil())
		Expect(project.ArchivedAt).To(BeNil(), "a hold is not an archive")

		By("and the count is the open work that left the ready set")
		Expect(held).To(Equal(2), "the done node was never in the ready set")
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(BeEmpty())
		Expect(g.statuses(ctx, "#1", "#2")).To(Equal([]string{"open", "open"}), "a hold changes no status")

		By("and one event records who parked it and why")
		events := g.eventsFor(ctx, "hold-p-1")
		Expect(events).To(HaveLen(1))
		Expect(events[0].Op).To(Equal(domain.OpHoldProject))
		Expect(events[0].PrincipalID).To(Equal(user.PrincipalID))
		Expect(events[0].Reason).To(HaveValue(Equal("not this quarter")))
		Expect(events[0].ProjectID).To(HaveValue(Equal(g.project)))
		Expect(events[0].NodeID).To(BeNil(), "the project is the subject, not any node")

		By("and repeating the request replays it without a second event")
		replayed, _, err := svc.HoldProject(ctx, g.org, user, "hold-p-1", "platform", "not this quarter")
		Expect(err).NotTo(HaveOccurred())
		Expect(replayed.HeldAt).To(Equal(project.HeldAt))
		Expect(g.eventsFor(ctx, "hold-p-1")).To(HaveLen(1))

		By("and the same request id with a different reason is a conflict")
		_, _, err = svc.HoldProject(ctx, g.org, user, "hold-p-1", "platform", "another reason")
		var conflict *domain.RequestConflictError
		Expect(errors.As(err, &conflict)).To(BeTrue(), "got %v", err)

		By("and holding a held project changes nothing and records nothing")
		same, again, err := svc.HoldProject(ctx, g.org, user, "hold-p-2", "platform", "still parked")
		Expect(err).NotTo(HaveOccurred())
		Expect(same.HeldReason).To(HaveValue(Equal("not this quarter")), "the first hold stands")
		Expect(again).To(BeZero())
		Expect(g.eventsFor(ctx, "hold-p-2")).To(BeEmpty())
	})

	It("Releasing a project's hold returns its work, once", func(ctx SpecContext) {
		By("given a held project with two open nodes")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
		})
		_, _, err := svc.HoldProject(ctx, g.org, user, "hold-p-1", "platform", "not this quarter")
		Expect(err).NotTo(HaveOccurred())
		Expect(g.explain(ctx, "#1").Reasons).To(HaveExactElements(HaveField("Kind", domain.ReasonProjectHeld)))

		By("when the user releases it")
		project, err := svc.UnholdProject(ctx, g.org, user, "unhold-p-1", "platform", "green light")
		Expect(err).NotTo(HaveOccurred())

		By("then the hold is gone and the work is ready")
		Expect(project.HeldAt).To(BeNil())
		Expect(project.HeldBy).To(BeNil())
		Expect(project.HeldReason).To(BeNil())
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1), int64(2)))
		Expect(g.explain(ctx, "#1").Reasons).To(BeEmpty())

		By("and the release is an event with its reason")
		events := g.eventsFor(ctx, "unhold-p-1")
		Expect(events).To(HaveLen(1))
		Expect(events[0].Op).To(Equal(domain.OpUnholdProject))
		Expect(events[0].Reason).To(HaveValue(Equal("green light")))
		Expect(events[0].PrincipalID).To(Equal(user.PrincipalID))

		By("and releasing a project with no hold changes nothing and records nothing")
		again, err := svc.UnholdProject(ctx, g.org, user, "unhold-p-2", "platform", "again")
		Expect(err).NotTo(HaveOccurred())
		Expect(again.HeldAt).To(BeNil())
		Expect(g.eventsFor(ctx, "unhold-p-2")).To(BeEmpty())
	})

	It("A project hold owes a request id, a project, and a reason, and is a person's to place", func(ctx SpecContext) {
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
		})

		By("when the request is missing what every hold owes")
		_, _, err := svc.HoldProject(ctx, g.org, user, "", "platform", "parked")
		Expect(err).To(MatchError(domain.ErrNoRequestID))
		_, _, err = svc.HoldProject(ctx, g.org, user, "hold-1", "", "parked")
		Expect(err).To(MatchError(domain.ErrProjectRequired))
		_, _, err = svc.HoldProject(ctx, g.org, user, "hold-1", "platform", "")
		Expect(err).To(MatchError(domain.ErrReasonRequired))
		_, err = svc.UnholdProject(ctx, g.org, user, "unhold-1", "platform", "")
		Expect(err).To(MatchError(domain.ErrReasonRequired))
		_, _, err = svc.HoldProject(ctx, g.org, user, "hold-1", "nowhere", "parked")
		Expect(err).To(MatchError(domain.ErrProjectNotFound))

		By("and when an agent tries to park a project")
		agent := g.agent(ctx)
		_, _, err = svc.HoldProject(ctx, g.org, agent, "hold-agent", "platform", "parked")

		By("then it is refused, naming the action, and nothing is parked")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no rule permits agent hold_project"))
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(1)))
		Expect(g.eventsFor(ctx, "hold-agent")).To(BeEmpty())
	})

	It("A sweep holds a parked epic once, and containment carries it to the children", func(ctx SpecContext) {
		By("given an epic with a child, and an unrelated root")
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			one := g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, &one)
			g.node(ctx, tx, 3, nil)
		})

		By("when the sweep keeps only the root")
		result, err := svc.Focus(ctx, g.org, user, "focus-1", "platform", []string{"#3"}, "this week")
		Expect(err).NotTo(HaveOccurred())

		By("then the epic is held and its child is parked through it, with no hold of its own")
		Expect(result.Held).To(Equal(1))
		Expect(g.getNode(ctx, "#1").HeldAt).NotTo(BeNil())
		Expect(g.getNode(ctx, "#2").HeldAt).To(BeNil())
		e := g.explain(ctx, "#2")
		Expect(e.Reasons).To(HaveExactElements(HaveField("Kind", domain.ReasonHeld)))
		Expect(e.Reasons[0].Hold.Number).To(BeEquivalentTo(1))
		Expect(e.Reasons[0].Hold.Self).To(BeFalse())
		Expect(numbers(g.ready(ctx, domain.ReadyQuery{}))).To(ConsistOf(int64(3)))
	})

	It("A sweep keeps only nodes of its own project, is bounded, and clears by the group it named", func(ctx SpecContext) {
		user := g.principal(ctx)
		other := g.newProject(ctx, "other")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.node(ctx, tx, 2, nil)
			g.nodeIn(ctx, tx, other, "other", 1, nil)
		})

		By("when the keep-set names a node of another project")
		_, err := svc.Focus(ctx, g.org, user, "focus-1", "platform", []string{"other#1"}, "this week")
		Expect(err).To(MatchError(ContainSubstring("other#1 is not in project platform")))
		Expect(g.eventsFor(ctx, "focus-1")).To(BeEmpty())

		By("and when it names more than the ceiling allows")
		keep := make([]string, domain.MaxKeepSet+1)
		for i := range keep {
			keep[i] = "#1"
		}
		_, err = svc.Focus(ctx, g.org, user, "focus-2", "platform", keep, "this week")
		Expect(err).To(HaveOccurred())
		Expect(g.eventsFor(ctx, "focus-2")).To(BeEmpty())

		By("and when it keeps everything, nothing is parked and nothing recorded")
		result, err := svc.Focus(ctx, g.org, user, "focus-3", "platform", []string{"#1", "#2"}, "all of it")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Held).To(BeZero())
		Expect(g.eventsFor(ctx, "focus-3")).To(BeEmpty())

		By("and a sweep is cleared by the group it reported")
		result, err = svc.Focus(ctx, g.org, user, "focus-4", "platform", []string{"#1"}, "one thing")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Held).To(Equal(1))
		released, err := svc.ClearFocus(ctx, g.org, user, "clear-4", "platform", result.Group, "done")
		Expect(err).NotTo(HaveOccurred())
		Expect(released).To(Equal(1))
		released, err = svc.ClearFocus(ctx, g.org, user, "clear-5", "platform", result.Group, "again")
		Expect(err).NotTo(HaveOccurred(), "clearing a sweep already cleared is a retry, not a mistake")
		Expect(released).To(BeZero())
	})

	It("An organization-wide focus parks every project outside the keep-set and leaves hand-placed holds alone", func(ctx SpecContext) {
		By("given three projects with open work, one of them parked by hand")
		user := g.principal(ctx)
		other := g.newProject(ctx, "other")
		parked := g.newProject(ctx, "parked")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.node(ctx, tx, 1, nil)
			g.nodeIn(ctx, tx, other, "other", 1, nil)
			g.nodeIn(ctx, tx, parked, "parked", 1, nil)
		})
		_, _, err := svc.HoldProject(ctx, g.org, user, "hold-parked", "parked", "parked in march")
		Expect(err).NotTo(HaveOccurred())

		By("when the organization focuses on platform")
		result, err := svc.Focus(ctx, g.org, user, "focus-org-1", "", []string{"platform"}, "platform week")
		Expect(err).NotTo(HaveOccurred())

		By("then only the project already held by hand is not the sweep's to take")
		Expect(result.Held).To(Equal(1), "other; parked was already held")
		Expect(result.Group).NotTo(BeEmpty())
		ready, err := g.readySet(ctx, domain.ReadyQuery{})
		Expect(err).NotTo(HaveOccurred())
		Expect(projectIDs(ready)).To(Equal([]string{g.project}))

		By("and the swept project's hold carries the sweep's group and the reason")
		e := g.explain(ctx, "other#1")
		Expect(e.Reasons).To(HaveExactElements(HaveField("Kind", domain.ReasonProjectHeld)))
		Expect(e.Reasons[0].Project.HoldGroup).To(HaveValue(Equal(result.Group)))
		Expect(e.Reasons[0].Project.HeldReason).To(HaveValue(Equal("platform week")))
		events := g.eventsFor(ctx, "focus-org-1")
		Expect(events).To(HaveLen(1))
		Expect(events[0].Op).To(Equal(domain.OpHoldProject))
		Expect(events[0].ProjectID).To(HaveValue(Equal(other)))
		Expect(events[0].Reason).To(HaveValue(Equal("platform week")))

		By("and a project created after the sweep is live, not held")
		late := g.newProject(ctx, "late")
		g.given(ctx, func(ctx context.Context, tx store.Tx) {
			g.nodeIn(ctx, tx, late, "late", 1, nil)
		})
		Expect(g.explain(ctx, "late#1").Reasons).To(BeEmpty())

		By("and clearing the focus releases the sweep's hold and no other")
		released, err := svc.ClearFocus(ctx, g.org, user, "clear-org-1", "", "", "week over")
		Expect(err).NotTo(HaveOccurred())
		Expect(released).To(Equal(1))
		Expect(g.explain(ctx, "other#1").Reasons).To(BeEmpty())
		e = g.explain(ctx, "parked#1")
		Expect(e.Reasons).To(HaveExactElements(HaveField("Kind", domain.ReasonProjectHeld)))
		Expect(e.Reasons[0].Project.HeldReason).To(HaveValue(Equal("parked in march")))
		Expect(e.Reasons[0].Project.HoldGroup).To(BeNil())
	})
})
