package server

import (
	"context"

	"connectrpc.com/connect/v2"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/server/interceptor"
)

// WorkService is the skift.v1.WorkService handler: the ready set, claims
// under a fencing token, heartbeats, and releases. Like GraphService it
// translates and decides nothing.
type WorkService struct {
	svc *domain.Service
}

var _ skiftv1connect.WorkServiceHandler = (*WorkService)(nil)

// NewWorkService returns the WorkService handler over svc.
func NewWorkService(svc *domain.Service) *WorkService {
	return &WorkService{svc: svc}
}

// Ready implements skiftv1connect.WorkServiceHandler.
func (w *WorkService) Ready(ctx context.Context, req *skiftv1.ReadyRequest) (*skiftv1.ReadyResponse, error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	f := req.GetFilter()
	if err := rejectStatusHeld(f); err != nil {
		return nil, err
	}
	offset, err := decodePageToken(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := pageSize(req.GetPageSize())
	nodes, held, err := w.svc.Ready(ctx, orgID, actor, f.GetProjects(), domain.ReadyQuery{
		Kinds:        f.GetKinds(),
		AssigneeKind: assigneeKinds.str(f.GetAssigneeKind()),
		Labels:       f.GetLabels(),
		AnyLabels:    f.GetAnyLabels(),
		MinPriority:  priorities.str(f.GetMinPriority()),
		Parent:       f.GetParent(),
		Limit:        size,
		Offset:       offset,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return &skiftv1.ReadyResponse{
		Nodes:         toNodes(nodes, viewOr(req.GetView(), skiftv1.View_VIEW_SUMMARY)),
		NextPageToken: nextPageToken(offset, size, len(nodes)),
		HeldCount:     int32(held),
	}, nil
}

// ClaimNext implements skiftv1connect.WorkServiceHandler. A waiting claim
// ends early, empty, when the server starts draining, so a shutdown never
// cuts a client off mid-wait and never interrupts a claim mid-transaction.
func (w *WorkService) ClaimNext(ctx context.Context, req *skiftv1.ClaimNextRequest) (*skiftv1.ClaimNextResponse, error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	f := req.GetFilter()
	if err := rejectStatusHeld(f); err != nil {
		return nil, err
	}
	wait, err := fromDuration(req.GetWait())
	if err != nil {
		return nil, err
	}
	ttl, err := fromDuration(req.GetTtl())
	if err != nil {
		return nil, err
	}
	out, err := w.svc.ClaimNext(ctx, orgID, actor, domain.ClaimRequest{
		RequestID:    req.GetRequestId(),
		Projects:     f.GetProjects(),
		Kinds:        f.GetKinds(),
		AssigneeKind: assigneeKinds.str(f.GetAssigneeKind()),
		Labels:       f.GetLabels(),
		AnyLabels:    f.GetAnyLabels(),
		MinPriority:  priorities.str(f.GetMinPriority()),
		Parent:       f.GetParent(),
		TTL:          ttl,
		Wait:         wait,
		Harness:      req.GetHarness(),
		AgentRef:     req.GetAgentRef(),
		SessionRef:   req.GetSessionRef(),
		Stop:         Draining(ctx),
	})
	if err != nil {
		return nil, connectError(err)
	}
	res := &skiftv1.ClaimNextResponse{}
	if out != nil {
		res.Node = toNode(*out.Node)
		res.Lease = toLease(out.Lease)
		res.Inputs = toInputsHint(out.Inputs)
		res.Comments = toCommentsHint(out.Comments)
		res.Project = toProject(*out.Project)
	}
	return res, nil
}

// Heartbeat implements skiftv1connect.WorkServiceHandler.
func (w *WorkService) Heartbeat(ctx context.Context, req *skiftv1.HeartbeatRequest) (*skiftv1.HeartbeatResponse, error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	ttl, err := fromDuration(req.GetTtl())
	if err != nil {
		return nil, err
	}
	change, err := w.svc.Heartbeat(ctx, orgID, actor, req.GetRequestId(), req.GetLeaseId(), ttl)
	if err != nil {
		return nil, connectError(err)
	}
	return &skiftv1.HeartbeatResponse{Lease: toLease(change.Lease)}, nil
}

// ReportUsage implements skiftv1connect.WorkServiceHandler. The report
// is applied as a one-operation patch carrying the attempt's lease, so it
// is fenced, idempotent, and evented like every other write to a held
// node.
func (w *WorkService) ReportUsage(ctx context.Context, req *skiftv1.ReportUsageRequest) (*skiftv1.ReportUsageResponse, error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := onlyOrchestrators(actor); err != nil {
		return nil, err
	}
	out, err := w.svc.Apply(ctx, orgID, actor, domain.Patch{
		RequestID: req.GetRequestId(),
		Project:   req.GetProject(),
		LeaseID:   req.GetLeaseId(),
		Ops: []domain.Op{{ReportUsage: &domain.ReportUsage{
			Node: req.GetNode(), Model: req.GetModel(),
			Tokens: domain.Tokens{
				InputTokens: req.GetInputTokens(), OutputTokens: req.GetOutputTokens(),
				CacheReadTokens: req.GetCacheReadTokens(), CacheWriteTokens: req.GetCacheWriteTokens(),
			},
		}}},
	})
	if err != nil {
		return nil, connectError(err)
	}
	res := &skiftv1.ReportUsageResponse{}
	if len(out.Usage) > 0 {
		res.Usage = toUsage(out.Usage[0])
	}
	return res, nil
}

// onlyOrchestrators refuses a principal that runs work rather than
// dispatching it. What an attempt cost is counted by the process running
// the agent, never by the agent's own account of itself, so ReportUsage
// is a door agents do not have; an agent says what work is expected to
// cost with the estimate_node operation of a patch instead. Only a user
// principal passes today: an orchestrator runs under the person whose
// work it dispatches, and no principal kind names an orchestrator.
func onlyOrchestrators(actor domain.Actor) error {
	if actor.PrincipalKind == string(interceptor.KindUser) {
		return nil
	}
	return connect.Errorf(connect.CodePermissionDenied, "the orchestrator reports usage, not the %s that does the work: "+
		"record what a node is expected to cost with the estimate_node operation of a patch, "+
		"and leave reporting what an attempt cost to whoever dispatched it", actor.PrincipalKind)
}

// Release implements skiftv1connect.WorkServiceHandler.
func (w *WorkService) Release(ctx context.Context, req *skiftv1.ReleaseRequest) (*skiftv1.ReleaseResponse, error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	change, err := w.svc.Release(ctx, orgID, actor, req.GetRequestId(), req.GetLeaseId(), releaseReasons.str(req.GetReason()), req.GetDetail())
	if err != nil {
		return nil, connectError(err)
	}
	return &skiftv1.ReleaseResponse{Lease: toLease(change.Lease)}, nil
}
