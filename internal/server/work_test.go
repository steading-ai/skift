package server_test

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("WorkService", Label(testsupport.DBLabel), func() {
	var f *fixture

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		f.project(ctx, "Platform")
	})

	Describe("Ready", func() {
		Describe("Scenario: The ready set is a summary", func() {
			It("The ready set is a summary", func(ctx SpecContext) {
				By("given 22 ready nodes with 2000-byte bodies")
				ops := make([]*daggerv1.PatchOp, 0, 22)
				for i := range 22 {
					ops = append(ops, createOpWith(&daggerv1.CreateNodeOp{
						Alias: fmt.Sprintf("n%d", i+1), Title: fmt.Sprintf("Task %d", i+1), Kind: "task",
						Priority: daggerv1.Priority_PRIORITY_P1, Body: strings.Repeat("b", 2000),
					}))
				}
				f.mustApply(ctx, ops...)

				By("when ready is called with no view")
				res, err := f.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{Projects: []string{"platform"}}}))
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Msg.GetNodes()).To(HaveLen(22))

				By("then every node in the response has title, kind, priority and reference")
				for i, n := range res.Msg.GetNodes() {
					Expect(n.GetTitle()).To(Equal(fmt.Sprintf("Task %d", i+1)))
					Expect(n.GetKind()).To(Equal("task"))
					Expect(n.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P1))
					Expect(n.GetStatus()).To(Equal(daggerv1.Status_STATUS_OPEN))
					Expect(n.GetReference()).To(Equal(fmt.Sprintf("platform#%d", i+1)))
					Expect(n.GetReady()).To(BeTrue())
				}

				By("and no node in the response has a body")
				for _, n := range res.Msg.GetNodes() {
					Expect(n.GetBody()).To(BeEmpty())
				}
				wire := wireJSON(res.Msg)
				Expect(wire).NotTo(ContainSubstring(`"body"`))
				Expect(len(wire)).To(BeNumerically("<", 22*2000), "the bodies were not sent: %d bytes", len(wire))

				By("and the full view carries the body on request")
				full, err := f.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{Projects: []string{"platform"}}, View: daggerv1.View_VIEW_FULL, PageSize: 1}))
				Expect(err).NotTo(HaveOccurred())
				Expect(full.Msg.GetNodes()[0].GetBody()).To(HaveLen(2000))
			})
		})

		It("returns the ready set most urgent first, filtered and paged", func(ctx SpecContext) {
			f.mustApply(ctx,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "low", Title: "Low", Priority: daggerv1.Priority_PRIORITY_P3}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "high", Title: "High", Priority: daggerv1.Priority_PRIORITY_P0, Labels: []string{"infra"}}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "blocked", Title: "Blocked", BlockedBy: []string{"low"}}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "gate", Title: "Gate", AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER}),
			)
			ready := func(req *daggerv1.ReadyRequest) *daggerv1.ReadyResponse {
				GinkgoHelper()
				if req.Filter == nil {
					req.Filter = &daggerv1.NodeFilter{}
				}
				req.Filter.Projects = []string{"platform"}
				res, err := f.work().Ready(ctx, connect.NewRequest(req))
				Expect(err).NotTo(HaveOccurred())
				return res.Msg
			}

			all := ready(&daggerv1.ReadyRequest{})
			Expect(numbers(all.GetNodes())).To(Equal([]int64{2, 4, 1}), "p0, then p2, then p3; the blocked node is out")
			for _, n := range all.GetNodes() {
				Expect(n.GetReady()).To(BeTrue())
			}
			Expect(numbers(ready(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{MinPriority: daggerv1.Priority_PRIORITY_P2}}).GetNodes())).To(Equal([]int64{2, 4}))
			Expect(numbers(ready(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{Labels: []string{"infra"}}}).GetNodes())).To(Equal([]int64{2}))
			Expect(numbers(ready(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER}}).GetNodes())).To(Equal([]int64{4}))
			Expect(numbers(ready(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_AGENT}}).GetNodes())).To(Equal([]int64{2, 1}))

			page := ready(&daggerv1.ReadyRequest{PageSize: 2})
			Expect(numbers(page.GetNodes())).To(Equal([]int64{2, 4}))
			Expect(page.GetNextPageToken()).NotTo(BeEmpty())
			last := ready(&daggerv1.ReadyRequest{PageSize: 2, PageToken: page.GetNextPageToken()})
			Expect(numbers(last.GetNodes())).To(Equal([]int64{1}))
			Expect(last.GetNextPageToken()).To(BeEmpty())

			res, err := f.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{}))
			Expect(err).NotTo(HaveOccurred(), "an empty filter means every project the caller may read, not an error")
			Expect(numbers(res.Msg.GetNodes())).To(Equal([]int64{2, 4, 1}), "platform is the caller's only project")
		})
	})

	Describe("ClaimNext", func() {
		It("hands out the most urgent ready node under a lease, then the next", func(ctx SpecContext) {
			f.mustApply(ctx,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "a", Title: "A", Priority: daggerv1.Priority_PRIORITY_P1}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "b", Title: "B", Priority: daggerv1.Priority_PRIORITY_P0}),
			)

			first := f.claim(ctx, "platform", nil)
			Expect(first.GetNode().GetNumber()).To(BeEquivalentTo(2))
			Expect(first.GetNode().GetClaimed()).To(BeTrue())
			Expect(first.GetNode().GetReady()).To(BeFalse())
			Expect(first.GetLease().GetId()).NotTo(BeEmpty())
			Expect(first.GetLease().GetNodeId()).To(Equal(first.GetNode().GetId()))
			Expect(first.GetLease().GetHolder()).To(Equal(f.agent.ID))
			Expect(first.GetLease().GetHarness()).To(Equal("spec"))
			Expect(first.GetLease().GetExpiresAt().AsTime()).To(BeTemporally(">", time.Now()))
			Expect(first.GetLease().GetReleaseReason()).To(Equal(daggerv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED))

			second := f.claim(ctx, "platform", nil)
			Expect(second.GetNode().GetNumber()).To(BeEquivalentTo(1))
			Expect(second.GetLease().GetId()).NotTo(Equal(first.GetLease().GetId()))
		})

		It("carries the project the node was claimed in, with the repository it delivers to", func(ctx SpecContext) {
			f.mustApply(ctx, createOpWith(&daggerv1.CreateNodeOp{Alias: "a", Title: "A"}))
			_, err := f.graph().UpdateProject(ctx, connect.NewRequest(&daggerv1.UpdateProjectRequest{
				RequestId: newID(), Project: "platform", VcsUrl: ptr("https://gitlab.example/x/y.git"),
			}))
			Expect(err).NotTo(HaveOccurred())

			claimed := f.claim(ctx, "platform", nil)
			Expect(claimed.GetProject().GetSlug()).To(Equal("platform"))
			Expect(claimed.GetProject().GetId()).To(Equal(claimed.GetNode().GetProjectId()))
			Expect(claimed.GetProject().GetVcsUrl()).To(Equal("https://gitlab.example/x/y.git"))
		})

		It("waits for work and returns empty when none appears", func(ctx SpecContext) {
			f.as(f.agent)
			started := time.Now()
			res, err := f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{
				RequestId: newID(), Filter: &daggerv1.NodeFilter{Projects: []string{"platform"}}, Wait: durationpb.New(150 * time.Millisecond), Harness: "spec", AgentRef: "spec",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetNode()).To(BeNil())
			Expect(res.Msg.GetLease()).To(BeNil())
			Expect(time.Since(started)).To(BeNumerically(">=", 150*time.Millisecond))
		})

		It("returns empty rather than erroring when the server drains mid-wait", func(ctx SpecContext) {
			f.as(f.agent)
			type outcome struct {
				res *daggerv1.ClaimNextResponse
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				defer GinkgoRecover()
				res, err := f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{
					RequestId: newID(), Filter: &daggerv1.NodeFilter{Projects: []string{"platform"}}, Wait: durationpb.New(time.Minute), Harness: "spec", AgentRef: "spec",
				}))
				if err != nil {
					done <- outcome{err: err}
					return
				}
				done <- outcome{res: res.Msg}
			}()
			time.Sleep(200 * time.Millisecond)

			Expect(f.run.stop()).To(Succeed())
			var got outcome
			Eventually(done, 5*time.Second).Should(Receive(&got))
			Expect(got.err).NotTo(HaveOccurred())
			Expect(got.res.GetNode()).To(BeNil())
		})

		It("reports a missing project as not found and a negative wait as invalid", func(ctx SpecContext) {
			f.as(f.agent)
			_, err := f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{RequestId: newID(), Filter: &daggerv1.NodeFilter{Projects: []string{"nowhere"}}, Harness: "spec", AgentRef: "spec"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			_, err = f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{RequestId: newID(), Filter: &daggerv1.NodeFilter{Projects: []string{"platform"}}, Wait: durationpb.New(-time.Second)}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("Heartbeat", func() {
		It("renews a live lease and refuses one that is not the caller's or not live", func(ctx SpecContext) {
			f.mustApply(ctx, createOp("a", "A"))
			claimed := f.claim(ctx, "platform", durationpb.New(time.Minute))

			f.as(f.user)
			_, err := f.work().Heartbeat(ctx, connect.NewRequest(&daggerv1.HeartbeatRequest{RequestId: newID(), LeaseId: claimed.GetLease().GetId()}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition), "held by another principal")

			f.as(f.agent)
			renewed, err := f.work().Heartbeat(ctx, connect.NewRequest(&daggerv1.HeartbeatRequest{
				RequestId: newID(), LeaseId: claimed.GetLease().GetId(), Ttl: durationpb.New(30 * time.Minute),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(renewed.Msg.GetLease().GetExpiresAt().AsTime()).To(BeTemporally(">", claimed.GetLease().GetExpiresAt().AsTime()))

			_, err = f.work().Release(ctx, connect.NewRequest(&daggerv1.ReleaseRequest{
				RequestId: newID(), LeaseId: claimed.GetLease().GetId(), Reason: daggerv1.ReleaseReason_RELEASE_REASON_CRASH,
			}))
			Expect(err).NotTo(HaveOccurred())
			_, err = f.work().Heartbeat(ctx, connect.NewRequest(&daggerv1.HeartbeatRequest{RequestId: newID(), LeaseId: claimed.GetLease().GetId()}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition), "no longer live")
		})
	})

	Describe("Release", func() {
		It("ends the lease with its reason and returns the node to the ready set", func(ctx SpecContext) {
			f.mustApply(ctx, createOp("a", "A"))
			claimed := f.claim(ctx, "platform", nil)

			f.as(f.agent)
			released, err := f.work().Release(ctx, connect.NewRequest(&daggerv1.ReleaseRequest{
				RequestId: newID(), LeaseId: claimed.GetLease().GetId(), Reason: daggerv1.ReleaseReason_RELEASE_REASON_GATE, Detail: "waiting on review",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(released.Msg.GetLease().GetReleaseReason()).To(Equal(daggerv1.ReleaseReason_RELEASE_REASON_GATE))

			ready, err := f.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{Filter: &daggerv1.NodeFilter{Projects: []string{"platform"}}}))
			Expect(err).NotTo(HaveOccurred())
			Expect(numbers(ready.Msg.GetNodes())).To(Equal([]int64{1}))

			_, err = f.work().Release(ctx, connect.NewRequest(&daggerv1.ReleaseRequest{RequestId: newID(), LeaseId: claimed.GetLease().GetId()}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "a reason is required")
		})
	})
})
