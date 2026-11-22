package server_test

import (
	"encoding/base64"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Ready hold blockers", Label(testsupport.DBLabel), func() {
	var f *fixture

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		f.project(ctx, "Platform")
	})

	ready := func(ctx SpecContext, filter *skiftv1.NodeFilter) *skiftv1.ReadyResponse {
		GinkgoHelper()
		res, err := f.work().Ready(ctx, &skiftv1.ReadyRequest{Filter: filter})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	It("finds a held gate through a diamond and counts each affected ticket once", func(ctx SpecContext) {
		f.mustApply(ctx,
			createOpWith(&skiftv1.CreateNodeOp{Alias: "gate", Title: "Wait for readings", AssigneeKind: skiftv1.AssigneeKind_ASSIGNEE_KIND_USER}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "left", Title: "A", BlockedBy: []string{"gate"}}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "right", Title: "B", BlockedBy: []string{"gate"}}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "join", Title: "C", BlockedBy: []string{"left", "right"}}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "last", Title: "D", BlockedBy: []string{"join"}, Labels: []string{"shipping"}}),
			&skiftv1.PatchOp{Op: &skiftv1.PatchOp_HoldNode{HoldNode: &skiftv1.HoldNodeOp{Node: "gate", Reason: "need more readings"}}},
		)
		f.as(f.agent)
		filter := &skiftv1.NodeFilter{Projects: []string{"platform"}, AssigneeKind: skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT}
		res := ready(ctx, filter)
		Expect(res.GetNodes()).To(BeEmpty())
		Expect(res.GetHoldBlockerCount()).To(BeEquivalentTo(1))
		Expect(res.GetHoldBlockers()).To(HaveLen(1))
		hold := res.GetHoldBlockers()[0]
		Expect(hold.GetReference()).To(Equal("platform#1"))
		Expect(hold.GetReason()).To(Equal("need more readings"))
		Expect(hold.GetDownstreamCount()).To(BeEquivalentTo(4))
		Expect(hold.GetDownstreamReferences()).To(Equal([]string{"platform#2", "platform#3", "platform#4"}))

		filter.Labels = []string{"shipping"}
		filtered := ready(ctx, filter)
		Expect(filtered.GetHoldBlockers()).To(HaveLen(1))
		Expect(filtered.GetHoldBlockers()[0].GetDownstreamCount()).To(BeEquivalentTo(1))
		Expect(filtered.GetHoldBlockers()[0].GetDownstreamReferences()).To(Equal([]string{"platform#5"}))

		claim, err := f.work().ClaimNext(ctx, &skiftv1.ClaimNextRequest{
			Filter: filter, RequestId: newID(), Harness: "spec", AgentRef: "spec",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.GetNode()).To(BeNil())
		later, err := f.work().Ready(ctx, &skiftv1.ReadyRequest{
			Filter: filter, PageToken: base64.RawURLEncoding.EncodeToString([]byte("o=1")),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(later.GetHoldBlockers()).To(BeEmpty())
		Expect(later.GetHoldBlockerCount()).To(BeZero())
	})

	It("never reveals holds or fallout outside the selected projects", func(ctx SpecContext) {
		f.project(ctx, "Delivery")
		f.mustApply(ctx, createOp("gate", "Wait"))
		_, err := f.apply(ctx, "delivery", createOpWith(&skiftv1.CreateNodeOp{
			Alias: "deploy", Title: "Deploy", BlockedBy: []string{"platform#1"},
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.graph().HoldProject(ctx, &skiftv1.HoldProjectRequest{
			RequestId: newID(), Project: "platform", Reason: "paused for review",
		})
		Expect(err).NotTo(HaveOccurred())
		f.as(f.agent)

		excluded := ready(ctx, &skiftv1.NodeFilter{Projects: []string{"delivery"}})
		Expect(excluded.GetNodes()).To(BeEmpty())
		Expect(excluded.GetHeldCount()).To(BeZero())
		Expect(excluded.GetHoldBlockers()).To(BeEmpty())
		Expect(excluded.GetHoldBlockerCount()).To(BeZero())

		included := ready(ctx, &skiftv1.NodeFilter{Projects: []string{"platform"}})
		Expect(included.GetHoldBlockers()).To(HaveLen(1))
		Expect(included.GetHoldBlockers()[0].GetReference()).To(Equal("platform"))
		Expect(included.GetHoldBlockers()[0].GetDownstreamReferences()).To(Equal([]string{"platform#1"}))

		all := ready(ctx, nil)
		Expect(all.GetNodes()).To(BeEmpty())
		Expect(all.GetHoldBlockers()).To(HaveLen(1))
		Expect(all.GetHoldBlockers()[0].GetReason()).To(Equal("paused for review"))
		Expect(all.GetHoldBlockers()[0].GetDownstreamCount()).To(BeEquivalentTo(2))
		Expect(all.GetHoldBlockers()[0].GetDownstreamReferences()).To(Equal([]string{"delivery#1", "platform#1"}))
	})

	It("reports inherited holds and follows open children back up to dependents", func(ctx SpecContext) {
		f.mustApply(ctx,
			createOp("group", "Group"),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "subgroup", Parent: "group", Title: "Subgroup"}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "child", Parent: "subgroup", Title: "Child"}),
			createOpWith(&skiftv1.CreateNodeOp{Alias: "dependent", Title: "Follow-up", BlockedBy: []string{"group"}}),
			&skiftv1.PatchOp{Op: &skiftv1.PatchOp_HoldNode{HoldNode: &skiftv1.HoldNodeOp{Node: "subgroup", Reason: "park this subtree"}}},
		)
		res := ready(ctx, nil)
		Expect(res.GetNodes()).To(BeEmpty())
		Expect(res.GetHeldCount()).To(BeEquivalentTo(2))
		Expect(res.GetHoldBlockers()).To(HaveLen(1))
		hold := res.GetHoldBlockers()[0]
		Expect(hold.GetReference()).To(Equal("platform#2"))
		Expect(hold.GetDownstreamCount()).To(BeEquivalentTo(3))
		Expect(hold.GetDownstreamReferences()).To(Equal([]string{"platform#1", "platform#3", "platform#4"}))
	})

	It("distinguishes no remaining work and keeps a large hold summary bounded", func(ctx SpecContext) {
		empty := ready(ctx, nil)
		Expect(empty.GetNodes()).To(BeEmpty())
		Expect(empty.GetHeldCount()).To(BeZero())
		Expect(empty.GetHoldBlockers()).To(BeEmpty())
		Expect(empty.GetHoldBlockerCount()).To(BeZero())
		for i := 1; i <= 6; i++ {
			f.mustApply(ctx, createOp("work", fmt.Sprintf("Ticket %d", i)),
				&skiftv1.PatchOp{Op: &skiftv1.PatchOp_HoldNode{HoldNode: &skiftv1.HoldNodeOp{Node: "work", Reason: "not yet"}}},
			)
		}
		res := ready(ctx, nil)
		Expect(res.GetNodes()).To(BeEmpty())
		Expect(res.GetHoldBlockerCount()).To(BeEquivalentTo(6))
		Expect(res.GetHoldBlockers()).To(HaveLen(5))
		Expect(res.GetHoldBlockers()[0].GetDownstreamCount()).To(BeZero())

		f.mustApply(ctx, createOp("ready", "Unrelated ready work"))
		nonempty := ready(ctx, nil)
		Expect(nonempty.GetNodes()).To(HaveLen(1))
		Expect(nonempty.GetHoldBlockers()).To(BeEmpty())
		Expect(nonempty.GetHoldBlockerCount()).To(BeZero())
	})
})
