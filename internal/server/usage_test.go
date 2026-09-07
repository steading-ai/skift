package server_test

import (
	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/testsupport"
)

// Estimates and usage reports over the wire: an agent estimates through a
// patch, an orchestrator reports through an RPC of its own, and the node
// carries both when it is read.
var _ = Describe("Estimates and usage", Label(testsupport.DBLabel), func() {
	var f *fixture

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		f.project(ctx, "Platform")
	})

	Describe("the estimate_node operation", func() {
		It("stores the estimate and shows it on the node", func(ctx SpecContext) {
			By("given a node")
			f.mustApply(ctx, createOp("work", "Do the work"))

			By("when a patch estimates it")
			f.mustApply(ctx, estimateOp(&daggerv1.EstimateNodeOp{
				Node: "#1", Model: "claude-opus-5", Basis: "two nodes of the same shape",
				InputTokens: 120000, OutputTokens: 8000, CacheReadTokens: 500, CacheWriteTokens: 250,
			}))

			By("then GetNode carries the estimate as it was recorded")
			estimate := f.getNode(ctx, "platform", "#1").GetEstimate()
			Expect(estimate).NotTo(BeNil())
			Expect(estimate.GetKind()).To(Equal(daggerv1.UsageKind_USAGE_KIND_ESTIMATE))
			Expect(estimate.GetModel()).To(Equal("claude-opus-5"))
			Expect(estimate.GetBasis()).To(Equal("two nodes of the same shape"))
			Expect(estimate.GetInputTokens()).To(Equal(int64(120000)))
			Expect(estimate.GetOutputTokens()).To(Equal(int64(8000)))
			Expect(estimate.GetCacheReadTokens()).To(Equal(int64(500)))
			Expect(estimate.GetCacheWriteTokens()).To(Equal(int64(250)))
			Expect(estimate.GetPrincipal()).To(Equal(f.user.ID))

			By("and the estimate rollup sums the current estimate of the subtree")
			rollup := f.getNode(ctx, "platform", "#1").GetUsage().GetEstimate()
			Expect(rollup).To(HaveLen(1))
			Expect(rollup[0].GetModel()).To(Equal("claude-opus-5"))
			Expect(rollup[0].GetRecords()).To(Equal(int64(1)))
			Expect(rollup[0].GetInputTokens()).To(Equal(int64(120000)))
		})

		It("re-estimating supersedes the earlier estimate", func(ctx SpecContext) {
			f.mustApply(ctx, createOp("work", "Do the work"))
			f.mustApply(ctx, estimateOp(&daggerv1.EstimateNodeOp{Node: "#1", Basis: "first pass", InputTokens: 1000}))
			f.mustApply(ctx, estimateOp(&daggerv1.EstimateNodeOp{Node: "#1", Basis: "second pass", InputTokens: 4000}))

			node := f.getNode(ctx, "platform", "#1")
			Expect(node.GetEstimate().GetBasis()).To(Equal("second pass"))
			Expect(node.GetEstimate().GetInputTokens()).To(Equal(int64(4000)))
			Expect(node.GetUsage().GetEstimate()).To(HaveLen(1))
			Expect(node.GetUsage().GetEstimate()[0].GetInputTokens()).To(Equal(int64(4000)))
		})

		It("refuses an estimate with no basis", func(ctx SpecContext) {
			f.mustApply(ctx, createOp("work", "Do the work"))
			_, err := f.apply(ctx, "platform", estimateOp(&daggerv1.EstimateNodeOp{Node: "#1", InputTokens: 10}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("basis"))
		})
	})

	Describe("ReportUsage", func() {
		It("stores a usage record for the lease and rolls it up on the node", func(ctx SpecContext) {
			By("given a node claimed by the orchestrator that dispatches it")
			f.mustApply(ctx, createOp("work", "Do the work"))
			lease := f.claimAsUser(ctx, "platform").GetLease()

			By("when it reports what the attempt has cost")
			res, err := f.work().ReportUsage(ctx, connect.NewRequest(&daggerv1.ReportUsageRequest{
				RequestId: newID(), Project: "platform", LeaseId: lease.GetId(), Node: "#1",
				Model: "claude-opus-5", InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 60, CacheWriteTokens: 20,
			}))
			Expect(err).NotTo(HaveOccurred())

			By("then the record it stored is attributed to that attempt")
			stored := res.Msg.GetUsage()
			Expect(stored.GetKind()).To(Equal(daggerv1.UsageKind_USAGE_KIND_ACTUAL))
			Expect(stored.GetLeaseId()).To(Equal(lease.GetId()))
			Expect(stored.GetPrincipal()).To(Equal(f.user.ID))
			Expect(stored.GetInputTokens()).To(Equal(int64(1200)))

			By("and GetNode shows the rollup of what the node has cost")
			actual := f.getNode(ctx, "platform", "#1").GetUsage().GetActual()
			Expect(actual).To(HaveLen(1))
			Expect(actual[0].GetModel()).To(Equal("claude-opus-5"))
			Expect(actual[0].GetRecords()).To(Equal(int64(1)))
			Expect(actual[0].GetInputTokens()).To(Equal(int64(1200)))
			Expect(actual[0].GetOutputTokens()).To(Equal(int64(340)))
			Expect(actual[0].GetCacheReadTokens()).To(Equal(int64(60)))
			Expect(actual[0].GetCacheWriteTokens()).To(Equal(int64(20)))

			By("and reporting again adds to the attempt rather than replacing it")
			_, err = f.work().ReportUsage(ctx, connect.NewRequest(&daggerv1.ReportUsageRequest{
				RequestId: newID(), Project: "platform", LeaseId: lease.GetId(), Node: "#1",
				Model: "claude-opus-5", InputTokens: 800,
			}))
			Expect(err).NotTo(HaveOccurred())
			actual = f.getNode(ctx, "platform", "#1").GetUsage().GetActual()
			Expect(actual).To(HaveLen(1))
			Expect(actual[0].GetRecords()).To(Equal(int64(2)))
			Expect(actual[0].GetInputTokens()).To(Equal(int64(2000)))
		})

		It("refuses an agent principal, saying the orchestrator reports usage", func(ctx SpecContext) {
			By("given a node claimed by an agent")
			f.mustApply(ctx, createOp("work", "Do the work"))
			lease := f.claim(ctx, "platform", nil).GetLease()

			By("when the agent reports what its own attempt cost")
			f.as(f.agent)
			defer f.as(f.user)
			_, err := f.work().ReportUsage(ctx, connect.NewRequest(&daggerv1.ReportUsageRequest{
				RequestId: newID(), Project: "platform", LeaseId: lease.GetId(), Node: "#1", InputTokens: 1200,
			}))

			By("then it is refused, and told what it may do instead")
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring("the orchestrator reports usage"),
				ContainSubstring("estimate_node"),
			))

			By("and nothing was recorded on the node")
			f.as(f.user)
			Expect(f.getNode(ctx, "platform", "#1").GetUsage().GetActual()).To(BeEmpty())
		})

		It("refuses a report carrying no lease", func(ctx SpecContext) {
			f.mustApply(ctx, createOp("work", "Do the work"))
			_, err := f.work().ReportUsage(ctx, connect.NewRequest(&daggerv1.ReportUsageRequest{
				RequestId: newID(), Project: "platform", Node: "#1", InputTokens: 1200,
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("lease"))
		})
	})
})
