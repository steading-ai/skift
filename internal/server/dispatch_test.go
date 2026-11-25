package server_test

import (
	"context"
	"sync"

	"connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("In-process Connect domain boundary", Label(testsupport.DBLabel), func() {
	var graph skiftv1connect.GraphServiceClient
	var work skiftv1connect.WorkServiceClient
	var a, b interceptor.Principal
	var aCtx, bCtx, otherCtx context.Context
	BeforeEach(func(ctx SpecContext) {
		pool := pg.Database(ctx)
		org := newID()
		a = interceptor.Principal{ID: newID(), OrgID: org, Kind: interceptor.KindAgent}
		b = interceptor.Principal{ID: newID(), OrgID: org, Kind: interceptor.KindAgent}
		_, err := pool.Exec(ctx, `INSERT INTO organizations(id,name) VALUES($1,'dispatch')`, org)
		Expect(err).NotTo(HaveOccurred())
		other := interceptor.Principal{ID: newID(), OrgID: newID(), Kind: interceptor.KindUser}
		_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name) VALUES($1,'other')`, other.OrgID)
		Expect(err).NotTo(HaveOccurred())
		for node, p := range map[string]interceptor.Principal{"a": a, "b": b, "other": other} {
			_, err = pool.Exec(ctx, `INSERT INTO principals(id,org_id,kind) VALUES($1,$2,$3)`, p.ID, p.OrgID, string(p.Kind))
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `INSERT INTO principal_identities(principal_id,scheme,value) VALUES($1,'tailscale',$2)`, p.ID, node)
			Expect(err).NotTo(HaveOccurred())
		}
		svc := domain.NewService(testsupport.Store(pool), pol)
		resolver := interceptor.TailscaleResolver{WhoIs: interceptor.WhoIsFunc(func(_ context.Context, peer string) (string, error) { return peer, nil })}
		srv := server.New(server.Config{Interceptors: []connect.ServerInterceptor{interceptor.NewPrincipal(server.NewIdentityLookup(svc), resolver)}}, server.Services{Graph: server.NewGraphService(svc), Work: server.NewWorkService(svc)}, pool)
		graph = skiftv1connect.NewGraphServiceClient(srv.InProcessClient())
		work = skiftv1connect.NewWorkServiceClient(srv.InProcessClient())
		otherCtx = interceptor.WithTailnetPeer(ctx, "other")
		aCtx = interceptor.WithTailnetPeer(ctx, "a")
		bCtx = interceptor.WithTailnetPeer(ctx, "b")
		_, err = graph.CreateProject(aCtx, &skiftv1.CreateProjectRequest{RequestId: newID(), Title: "Platform"})
		Expect(err).NotTo(HaveOccurred())
	})
	It("scopes resolved project references to the authenticated organization", func(ctx SpecContext) {
		aCtx = interceptor.WithTailnetPeer(ctx, "a")
		bCtx = interceptor.WithTailnetPeer(ctx, "b")
		otherCtx = interceptor.WithTailnetPeer(ctx, "other")
		_, err := graph.GetProject(otherCtx, &skiftv1.GetProjectRequest{Project: "platform"})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		_, err = graph.ApplyPatch(otherCtx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", Ops: []*skiftv1.PatchOp{createOp("x", "cross-org")}})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
	})
	It("attributes concurrent writes and leases to the actual node principal and identity scheme", func(ctx SpecContext) {
		aCtx = interceptor.WithTailnetPeer(ctx, "a")
		bCtx = interceptor.WithTailnetPeer(ctx, "b")
		otherCtx = interceptor.WithTailnetPeer(ctx, "other")
		var wg sync.WaitGroup
		for _, call := range []struct {
			ctx context.Context
			p   interceptor.Principal
		}{{aCtx, a}, {bCtx, b}} {
			wg.Go(func() {
				defer GinkgoRecover()
				res, err := graph.ApplyPatch(call.ctx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", View: skiftv1.View_VIEW_FULL, Ops: []*skiftv1.PatchOp{createOp("task", "Work")}})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.GetEvents()).To(HaveLen(1))
				Expect(res.GetEvents()[0].GetPrincipal()).To(Equal(call.p.ID))
				Expect(res.GetEvents()[0].GetIdentityScheme()).To(Equal(interceptor.SchemeTailscale))
				claim, err := work.ClaimNext(call.ctx, &skiftv1.ClaimNextRequest{RequestId: newID(), Filter: &skiftv1.NodeFilter{Projects: []string{"platform"}}, Harness: "spec", AgentRef: "spec"})
				Expect(err).NotTo(HaveOccurred())
				Expect(claim.GetLease().GetHolder()).To(Equal(call.p.ID))
			})
		}
		wg.Wait()
	})
	It("refuses another principal's heartbeat, release and completion even with the valid lease token", func(ctx SpecContext) {
		aCtx = interceptor.WithTailnetPeer(ctx, "a")
		bCtx = interceptor.WithTailnetPeer(ctx, "b")
		otherCtx = interceptor.WithTailnetPeer(ctx, "other")
		_, err := graph.ApplyPatch(aCtx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", Ops: []*skiftv1.PatchOp{createOp("task", "Work")}})
		Expect(err).NotTo(HaveOccurred())
		claim, err := work.ClaimNext(aCtx, &skiftv1.ClaimNextRequest{RequestId: newID(), Filter: &skiftv1.NodeFilter{Projects: []string{"platform"}}, Harness: "spec", AgentRef: "spec", SessionRef: "same-session"})
		Expect(err).NotTo(HaveOccurred())
		token := claim.GetLease().GetId()
		_, err = work.Heartbeat(bCtx, &skiftv1.HeartbeatRequest{RequestId: newID(), LeaseId: token})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring(domain.ErrNotLeaseHolder.Error()))
		_, err = work.Release(bCtx, &skiftv1.ReleaseRequest{RequestId: newID(), LeaseId: token, Reason: skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring(domain.ErrNotLeaseHolder.Error()))
		_, err = graph.ApplyPatch(bCtx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", LeaseId: token, Ops: []*skiftv1.PatchOp{completeOp("#1", "stolen")}})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring(domain.ErrNotLeaseHolder.Error()))
		got, err := graph.GetNode(aCtx, &skiftv1.GetNodeRequest{Project: "platform", Node: "#1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetLease().GetHolder()).To(Equal(a.ID))
		Expect(got.GetNode().GetStatus()).To(Equal(skiftv1.Status_STATUS_OPEN))
	})
	It("retains resolved patch denial and rolls back earlier allowed operations", func(ctx SpecContext) {
		aCtx = interceptor.WithTailnetPeer(ctx, "a")
		bCtx = interceptor.WithTailnetPeer(ctx, "b")
		otherCtx = interceptor.WithTailnetPeer(ctx, "other")
		_, err := graph.ApplyPatch(aCtx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", Ops: []*skiftv1.PatchOp{createOp("root", "A's root")}})
		Expect(err).NotTo(HaveOccurred())
		_, err = graph.ApplyPatch(bCtx, &skiftv1.ApplyPatchRequest{RequestId: newID(), Project: "platform", Ops: []*skiftv1.PatchOp{createOp("allowed", "B's root"), createOpWith(&skiftv1.CreateNodeOp{Alias: "denied", Title: "Unauthorized child", Parent: "#1"})}})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		nodes, err := graph.ListNodes(aCtx, &skiftv1.ListNodesRequest{Filter: &skiftv1.NodeFilter{Projects: []string{"platform"}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes.GetNodes()).To(HaveLen(1))
		Expect(nodes.GetNodes()[0].GetTitle()).To(Equal("A's root"))
		// A registered procedure still reaches its existing service-level guard.
		_, err = work.ReportUsage(bCtx, &skiftv1.ReportUsageRequest{RequestId: newID()})
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
	})
})
