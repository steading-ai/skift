package server_test

import (
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
		It("returns the ready set most urgent first, filtered and paged", func(ctx SpecContext) {
			f.mustApply(ctx,
				createOpWith(&daggerv1.CreateNodeOp{Alias: "low", Title: "Low", Priority: daggerv1.Priority_PRIORITY_P3}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "high", Title: "High", Priority: daggerv1.Priority_PRIORITY_P0, Labels: []string{"infra"}}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "blocked", Title: "Blocked", BlockedBy: []string{"low"}}),
				createOpWith(&daggerv1.CreateNodeOp{Alias: "gate", Title: "Gate", AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER}),
			)
			ready := func(req *daggerv1.ReadyRequest) *daggerv1.ReadyResponse {
				GinkgoHelper()
				req.Project = "platform"
				res, err := f.work().Ready(ctx, connect.NewRequest(req))
				Expect(err).NotTo(HaveOccurred())
				return res.Msg
			}

			all := ready(&daggerv1.ReadyRequest{})
			Expect(numbers(all.GetNodes())).To(Equal([]int64{2, 4, 1}), "p0, then p2, then p3; the blocked node is out")
			for _, n := range all.GetNodes() {
				Expect(n.GetReady()).To(BeTrue())
			}
			Expect(numbers(ready(&daggerv1.ReadyRequest{MinPriority: daggerv1.Priority_PRIORITY_P2}).GetNodes())).To(Equal([]int64{2, 4}))
			Expect(numbers(ready(&daggerv1.ReadyRequest{Labels: []string{"infra"}}).GetNodes())).To(Equal([]int64{2}))
			Expect(numbers(ready(&daggerv1.ReadyRequest{AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER}).GetNodes())).To(Equal([]int64{4}))
			Expect(numbers(ready(&daggerv1.ReadyRequest{AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_AGENT}).GetNodes())).To(Equal([]int64{2, 1}))

			page := ready(&daggerv1.ReadyRequest{PageSize: 2})
			Expect(numbers(page.GetNodes())).To(Equal([]int64{2, 4}))
			Expect(page.GetNextPageToken()).NotTo(BeEmpty())
			last := ready(&daggerv1.ReadyRequest{PageSize: 2, PageToken: page.GetNextPageToken()})
			Expect(numbers(last.GetNodes())).To(Equal([]int64{1}))
			Expect(last.GetNextPageToken()).To(BeEmpty())

			_, err := f.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "a project is required")
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

		It("waits for work and returns empty when none appears", func(ctx SpecContext) {
			f.as(f.agent)
			started := time.Now()
			res, err := f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{
				RequestId: newID(), Project: "platform", Wait: durationpb.New(150 * time.Millisecond), Harness: "spec", AgentRef: "spec",
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
					RequestId: newID(), Project: "platform", Wait: durationpb.New(time.Minute), Harness: "spec", AgentRef: "spec",
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
			_, err := f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{RequestId: newID(), Project: "nowhere", Harness: "spec", AgentRef: "spec"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			_, err = f.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{RequestId: newID(), Project: "platform", Wait: durationpb.New(-time.Second)}))
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

			ready, err := f.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{Project: "platform"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(numbers(ready.Msg.GetNodes())).To(Equal([]int64{1}))

			_, err = f.work().Release(ctx, connect.NewRequest(&daggerv1.ReleaseRequest{RequestId: newID(), LeaseId: claimed.GetLease().GetId()}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "a reason is required")
		})
	})
})
