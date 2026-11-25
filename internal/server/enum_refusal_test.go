package server_test

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

// A client built from a newer schema, or any binary request, sends an enum
// as its number. These specs send numbers no skift enum defines, the way
// such a request decodes on the server, and expect a refusal rather than
// the domain's default. See ADR-008.
const unknownEnum = 99

var _ = Describe("Enum values the server does not know", Label(testsupport.DBLabel), func() {
	var f *fixture

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		f.project(ctx, "Platform")
		f.mustApply(ctx, createOpWith(&skiftv1.CreateNodeOp{Alias: "a", Title: "Existing", Priority: skiftv1.Priority_PRIORITY_P1}))
	})

	// nodeCount is how many nodes the platform project holds.
	nodeCount := func(ctx context.Context) int {
		GinkgoHelper()
		res, err := f.graph().ListNodes(ctx, &skiftv1.ListNodesRequest{Filter: &skiftv1.NodeFilter{Projects: []string{"platform"}}})
		Expect(err).NotTo(HaveOccurred())
		return len(res.GetNodes())
	}

	// refused asserts err is the wire's InvalidArgument naming field and
	// the number it refused. The number tells it apart from a refusal the
	// domain makes later of the default an unknown value would have become,
	// such as a release with no reason.
	refused := func(err error, field string) {
		GinkgoHelper()
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("%s: %d is not a", field, unknownEnum)))
	}

	It("A gate from a newer client is refused, not created as agent work", func(ctx SpecContext) {
		By("given a CreateNode operation whose assignee kind is a number skift does not know")
		gate := createOpWith(&skiftv1.CreateNodeOp{Alias: "gate", Title: "Approve the release", AssigneeKind: skiftv1.AssigneeKind(unknownEnum)})

		By("when the patch is applied")
		_, err := f.apply(ctx, "platform", gate)

		By("then it is refused with InvalidArgument naming assignee_kind")
		refused(err, "assignee_kind")
		By("and no node is created")
		Expect(nodeCount(ctx)).To(Equal(1))
	})

	It("An unknown filter value is refused, not ignored", func(ctx SpecContext) {
		By("given ready nodes of several assignee kinds")
		f.mustApply(ctx, createOpWith(&skiftv1.CreateNodeOp{Alias: "g", Title: "Gate", AssigneeKind: skiftv1.AssigneeKind_ASSIGNEE_KIND_USER}))

		By("when I ask for ready nodes with an assignee kind skift does not know")
		_, err := f.work().Ready(ctx, &skiftv1.ReadyRequest{Filter: &skiftv1.NodeFilter{
			Projects: []string{"platform"}, AssigneeKind: skiftv1.AssigneeKind(unknownEnum),
		}})

		By("then the request is refused with InvalidArgument")
		refused(err, "filter.assignee_kind")
	})

	DescribeTable("refuses an unknown value in an operation, and applies none of the patch",
		func(ctx SpecContext, field string, bad *skiftv1.PatchOp) {
			_, err := f.apply(ctx, "platform", createOp("x", "Would be created first"), bad)
			refused(err, "ops[1]."+field)
			Expect(nodeCount(ctx)).To(Equal(1), "the valid operation ahead of it was not applied")
			Expect(f.getNode(ctx, "platform", "#1").GetNode().GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P1))
		},
		Entry("create priority", "create_node.priority",
			createOpWith(&skiftv1.CreateNodeOp{Alias: "b", Title: "B", Priority: skiftv1.Priority(unknownEnum)})),
		Entry("create assignee kind", "create_node.assignee_kind",
			createOpWith(&skiftv1.CreateNodeOp{Alias: "b", Title: "B", AssigneeKind: skiftv1.AssigneeKind(unknownEnum)})),
		Entry("update priority", "update_node.priority",
			updateOp(&skiftv1.UpdateNodeOp{Node: "#1", Priority: skiftv1.Priority(unknownEnum).Enum()})),
		Entry("update assignee kind", "update_node.assignee_kind",
			updateOp(&skiftv1.UpdateNodeOp{Node: "#1", AssigneeKind: skiftv1.AssigneeKind(unknownEnum).Enum()})),
		Entry("add edge kind", "add_edge.kind",
			&skiftv1.PatchOp{Op: &skiftv1.PatchOp_AddEdge{AddEdge: &skiftv1.AddEdgeOp{From: "#1", To: "x", Kind: skiftv1.EdgeKind(unknownEnum)}}}),
		Entry("remove edge kind", "remove_edge.kind",
			&skiftv1.PatchOp{Op: &skiftv1.PatchOp_RemoveEdge{RemoveEdge: &skiftv1.RemoveEdgeOp{From: "#1", To: "x", Kind: skiftv1.EdgeKind(unknownEnum)}}}),
		Entry("drop reason", "drop_node.reason",
			&skiftv1.PatchOp{Op: &skiftv1.PatchOp_DropNode{DropNode: &skiftv1.DropNodeOp{Node: "#1", Reason: skiftv1.DropReason(unknownEnum)}}}),
	)

	DescribeTable("refuses an unknown filter value on a read",
		func(ctx SpecContext, read string, field string, filter *skiftv1.NodeFilter) {
			filter.Projects = []string{"platform"}
			var err error
			switch read {
			case "list":
				_, err = f.graph().ListNodes(ctx, &skiftv1.ListNodesRequest{Filter: filter})
			case "ready":
				_, err = f.work().Ready(ctx, &skiftv1.ReadyRequest{Filter: filter})
			}
			refused(err, "filter."+field)
		},
		Entry("list status", "list", "status", &skiftv1.NodeFilter{Status: skiftv1.Status(unknownEnum)}),
		Entry("list assignee kind", "list", "assignee_kind", &skiftv1.NodeFilter{AssigneeKind: skiftv1.AssigneeKind(unknownEnum)}),
		Entry("list minimum priority", "list", "min_priority", &skiftv1.NodeFilter{MinPriority: skiftv1.Priority(unknownEnum)}),
		Entry("ready assignee kind", "ready", "assignee_kind", &skiftv1.NodeFilter{AssigneeKind: skiftv1.AssigneeKind(unknownEnum)}),
		Entry("ready minimum priority", "ready", "min_priority", &skiftv1.NodeFilter{MinPriority: skiftv1.Priority(unknownEnum)}),
	)

	DescribeTable("refuses an unknown filter value on a claim, and claims nothing",
		func(ctx SpecContext, field string, filter *skiftv1.NodeFilter) {
			filter.Projects = []string{"platform"}
			f.as(f.agent)
			_, err := f.work().ClaimNext(ctx, &skiftv1.ClaimNextRequest{RequestId: newID(), Filter: filter, Harness: "spec", AgentRef: "spec"})
			f.as(f.user)
			refused(err, "filter."+field)
			Expect(f.getNode(ctx, "platform", "#1").GetNode().GetClaimed()).To(BeFalse())
		},
		Entry("assignee kind", "assignee_kind", &skiftv1.NodeFilter{AssigneeKind: skiftv1.AssigneeKind(unknownEnum)}),
		Entry("minimum priority", "min_priority", &skiftv1.NodeFilter{MinPriority: skiftv1.Priority(unknownEnum)}),
	)

	It("refuses an unknown release reason, and leaves the lease live", func(ctx SpecContext) {
		lease := f.claim(ctx, "platform", durationpb.New(0)).GetLease()
		f.as(f.agent)
		defer f.as(f.user)

		_, err := f.work().Release(ctx, &skiftv1.ReleaseRequest{RequestId: newID(), LeaseId: lease.GetId(), Reason: skiftv1.ReleaseReason(unknownEnum)})
		refused(err, "reason")

		_, err = f.work().Heartbeat(ctx, &skiftv1.HeartbeatRequest{RequestId: newID(), LeaseId: lease.GetId()})
		Expect(err).NotTo(HaveOccurred(), "the refused release left the lease live")
	})

	It("refuses an unknown project priority, and creates no project", func(ctx SpecContext) {
		_, err := f.graph().CreateProject(ctx, &skiftv1.CreateProjectRequest{RequestId: newID(), Title: "Payments", Priority: skiftv1.Priority(unknownEnum)})
		refused(err, "priority")

		_, err = f.graph().GetProject(ctx, &skiftv1.GetProjectRequest{Project: "payments"})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("keeps the unspecified value's meaning: the default on a create, no filter on a read", func(ctx SpecContext) {
		res := f.mustApply(ctx, createOpWith(&skiftv1.CreateNodeOp{Alias: "d", Title: "Defaults"}))
		created := f.getNode(ctx, "platform", res.GetNodes()[0].GetReference()).GetNode()
		Expect(created.GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P2))
		Expect(created.GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED))

		Expect(nodeCount(ctx)).To(Equal(2), "an unspecified status, assignee kind and priority filter nothing")
	})
})
