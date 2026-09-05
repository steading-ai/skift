package server

import (
	"context"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/domain"
)

// WorkService is the dagger.v1.WorkService handler: the ready set, claims
// under a fencing token, heartbeats, and releases. Like GraphService it
// translates and decides nothing.
type WorkService struct {
	svc *domain.Service
}

var _ daggerv1connect.WorkServiceHandler = (*WorkService)(nil)

// NewWorkService returns the WorkService handler over svc.
func NewWorkService(svc *domain.Service) *WorkService {
	return &WorkService{svc: svc}
}

// Ready implements daggerv1connect.WorkServiceHandler.
func (w *WorkService) Ready(ctx context.Context, req *connect.Request[daggerv1.ReadyRequest]) (*connect.Response[daggerv1.ReadyResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	offset, err := decodePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	nodes, err := w.svc.Ready(ctx, orgID, req.Msg.GetProject(), domain.ReadyQuery{
		Kind:         req.Msg.GetKind(),
		AssigneeKind: assigneeKinds.str(req.Msg.GetAssigneeKind()),
		Labels:       req.Msg.GetLabels(),
		MinPriority:  priorities.str(req.Msg.GetMinPriority()),
		Limit:        size,
		Offset:       offset,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.ReadyResponse{
		Nodes:         toNodes(nodes),
		NextPageToken: nextPageToken(offset, size, len(nodes)),
	}), nil
}

// ClaimNext implements daggerv1connect.WorkServiceHandler. A waiting claim
// ends early, empty, when the server starts draining, so a shutdown never
// cuts a client off mid-wait and never interrupts a claim mid-transaction.
func (w *WorkService) ClaimNext(ctx context.Context, req *connect.Request[daggerv1.ClaimNextRequest]) (*connect.Response[daggerv1.ClaimNextResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	wait, err := fromDuration(req.Msg.GetWait())
	if err != nil {
		return nil, err
	}
	ttl, err := fromDuration(req.Msg.GetTtl())
	if err != nil {
		return nil, err
	}
	out, err := w.svc.ClaimNext(ctx, orgID, actor, domain.ClaimRequest{
		RequestID:    req.Msg.GetRequestId(),
		Project:      req.Msg.GetProject(),
		Kind:         req.Msg.GetKind(),
		AssigneeKind: assigneeKinds.str(req.Msg.GetAssigneeKind()),
		Labels:       req.Msg.GetLabels(),
		MinPriority:  priorities.str(req.Msg.GetMinPriority()),
		TTL:          ttl,
		Wait:         wait,
		Harness:      req.Msg.GetHarness(),
		AgentRef:     req.Msg.GetAgentRef(),
		SessionRef:   req.Msg.GetSessionRef(),
		Stop:         Draining(ctx),
	})
	if err != nil {
		return nil, connectError(err)
	}
	res := &daggerv1.ClaimNextResponse{}
	if out != nil {
		res.Node = toNode(*out.Node)
		res.Lease = toLease(out.Lease)
	}
	return connect.NewResponse(res), nil
}

// Heartbeat implements daggerv1connect.WorkServiceHandler.
func (w *WorkService) Heartbeat(ctx context.Context, req *connect.Request[daggerv1.HeartbeatRequest]) (*connect.Response[daggerv1.HeartbeatResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	ttl, err := fromDuration(req.Msg.GetTtl())
	if err != nil {
		return nil, err
	}
	change, err := w.svc.Heartbeat(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetLeaseId(), ttl)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.HeartbeatResponse{Lease: toLease(change.Lease)}), nil
}

// Release implements daggerv1connect.WorkServiceHandler.
func (w *WorkService) Release(ctx context.Context, req *connect.Request[daggerv1.ReleaseRequest]) (*connect.Response[daggerv1.ReleaseResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	change, err := w.svc.Release(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetLeaseId(), releaseReasons.str(req.Msg.GetReason()), req.Msg.GetDetail())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.ReleaseResponse{Lease: toLease(change.Lease)}), nil
}
