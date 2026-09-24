package server

import (
	"context"

	"connectrpc.com/connect"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/domain"
)

// GraphService is the skift.v1.GraphService handler. It translates each
// request into a domain call and the domain's answer into a response, and
// decides nothing about the graph itself. The actor of every call is the
// principal the transport resolved; nothing in a request names one.
type GraphService struct {
	svc *domain.Service
}

// The handler types implement the generated interfaces outright, without
// embedding the unimplemented defaults, so a proto RPC with no handler is a
// compile error rather than a runtime Unimplemented.
var _ skiftv1connect.GraphServiceHandler = (*GraphService)(nil)

// NewGraphService returns the GraphService handler over svc.
func NewGraphService(svc *domain.Service) *GraphService {
	return &GraphService{svc: svc}
}

// ApplyPatch implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) ApplyPatch(ctx context.Context, req *connect.Request[skiftv1.ApplyPatchRequest]) (*connect.Response[skiftv1.ApplyPatchResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	out, err := g.svc.Apply(ctx, orgID, actor, fromPatch(req.Msg))
	if err != nil {
		return nil, connectError(err)
	}
	view := viewOr(req.Msg.GetView(), skiftv1.View_VIEW_REFS)
	res := &skiftv1.ApplyPatchResponse{
		Nodes:      toNodes(out.States, view),
		Aliases:    out.Aliases,
		EventCount: int32(len(out.Events)), //nolint:gosec // bounded by the patch's operation ceiling
		RequestId:  req.Msg.GetRequestId(),
		// The undo list of a drop, cheap enough to carry under every
		// view: without it the caller cannot reopen what a cascade took.
		Dropped: out.Dropped,
	}
	// Events are the audit record, read through WatchEvents; a patch
	// response carries them only when asked for everything.
	if view == skiftv1.View_VIEW_FULL {
		if res.Events, err = toEvents(out.Events); err != nil {
			return nil, connectError(err)
		}
	}
	return connect.NewResponse(res), nil
}

// GetNode implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) GetNode(ctx context.Context, req *connect.Request[skiftv1.GetNodeRequest]) (*connect.Response[skiftv1.GetNodeResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	node, err := g.svc.GetNode(ctx, orgID, req.Msg.GetProject(), req.Msg.GetNode())
	if err != nil {
		return nil, connectError(err)
	}
	view := viewOr(req.Msg.GetView(), skiftv1.View_VIEW_FULL)
	res := &skiftv1.GetNodeResponse{Node: projectNode(toNode(node.NodeState), view), Inputs: toInputsHint(node.Inputs), CommentsHint: toCommentsHint(node.CommentsHint)}
	for _, e := range node.Edges {
		res.Edges = append(res.Edges, toEdge(e))
	}
	if node.Lease != nil {
		res.Lease = toLease(*node.Lease)
	}
	// The result, artifacts, and comments are content, like the body:
	// only the full view carries them.
	if view != skiftv1.View_VIEW_FULL {
		return connect.NewResponse(res), nil
	}
	if res.Result, err = toResult(node.Result, node.Artifacts); err != nil {
		return nil, connectError(err)
	}
	if res.Artifacts, err = toArtifacts(node.Artifacts); err != nil {
		return nil, connectError(err)
	}
	for _, c := range node.Comments {
		res.Comments = append(res.Comments, toComment(c))
	}
	// What the node is expected to cost and what it has cost: an
	// estimate an agent recorded, and the rollup over the node and its
	// descendants the domain computes on every read.
	if node.Estimate != nil {
		res.Estimate = toUsage(*node.Estimate)
	}
	res.Usage = toRollup(node.Usage)
	return connect.NewResponse(res), nil
}

// GetInputs implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) GetInputs(ctx context.Context, req *connect.Request[skiftv1.GetInputsRequest]) (*connect.Response[skiftv1.GetInputsResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	inputs, err := g.svc.GetInputs(ctx, orgID, actor, req.Msg.GetProject(), req.Msg.GetNode())
	if err != nil {
		return nil, connectError(err)
	}
	view := viewOr(req.Msg.GetView(), skiftv1.View_VIEW_FULL)
	res := &skiftv1.GetInputsResponse{Inputs: make([]*skiftv1.Input, 0, len(inputs))}
	for _, in := range inputs {
		wire := &skiftv1.Input{Node: projectNode(toNode(in.NodeState), view), Bytes: resultBytes(in.Result)}
		// The result is content, like a body: a narrower view says how
		// big it is and leaves fetching it to the caller.
		if view == skiftv1.View_VIEW_FULL {
			if wire.Result, err = toResult(in.Result, in.Artifacts); err != nil {
				return nil, connectError(err)
			}
		}
		res.Inputs = append(res.Inputs, wire)
	}
	return connect.NewResponse(res), nil
}

// ListNodes implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) ListNodes(ctx context.Context, req *connect.Request[skiftv1.ListNodesRequest]) (*connect.Response[skiftv1.ListNodesResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	offset, err := decodePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	f := req.Msg.GetFilter()
	nodes, err := g.svc.ListNodes(ctx, orgID, domain.NodeListQuery{
		Projects:     f.GetProjects(),
		Status:       statuses.str(f.GetStatus()),
		Kinds:        f.GetKinds(),
		AssigneeKind: assigneeKinds.str(f.GetAssigneeKind()),
		Labels:       f.GetLabels(),
		AnyLabels:    f.GetAnyLabels(),
		MinPriority:  priorities.str(f.GetMinPriority()),
		Parent:       f.GetParent(),
		Held:         f.GetHeld(),
		Limit:        size,
		Offset:       offset,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.ListNodesResponse{
		Nodes:         toNodes(nodes, viewOr(req.Msg.GetView(), skiftv1.View_VIEW_SUMMARY)),
		NextPageToken: nextPageToken(offset, size, len(nodes)),
	}), nil
}

// Explain implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) Explain(ctx context.Context, req *connect.Request[skiftv1.ExplainRequest]) (*connect.Response[skiftv1.ExplainResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	explained, err := g.svc.Explain(ctx, orgID, req.Msg.GetProject(), req.Msg.GetNode())
	if err != nil {
		return nil, connectError(err)
	}
	res := &skiftv1.ExplainResponse{Node: toNode(domain.NodeState{Node: explained.Node, Derived: explained.Derived[explained.Node.ID]})}
	for _, r := range explained.Reasons {
		res.Reasons = append(res.Reasons, toReason(r, explained.Derived))
	}
	return connect.NewResponse(res), nil
}

// Search implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) Search(ctx context.Context, req *connect.Request[skiftv1.SearchRequest]) (*connect.Response[skiftv1.SearchResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	offset, err := decodePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	hits, err := g.svc.Search(ctx, orgID, actor, domain.SearchQuery{
		Query:           req.Msg.GetQuery(),
		Project:         req.Msg.GetProject(),
		IncludeArchived: req.Msg.GetIncludeArchived(),
		Limit:           size,
		Offset:          offset,
	})
	if err != nil {
		return nil, connectError(err)
	}
	res := &skiftv1.SearchResponse{}
	view := viewOr(req.Msg.GetView(), skiftv1.View_VIEW_SUMMARY)
	for _, h := range hits {
		res.Hits = append(res.Hits, toHit(h, view))
	}
	// Policy may drop hits after paging, so a short page is not the last
	// one; the caller pages until a page comes back empty.
	if len(hits) > 0 {
		res.NextPageToken = encodePageToken(offset + size)
	}
	return connect.NewResponse(res), nil
}

// CreateProject implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) CreateProject(ctx context.Context, req *connect.Request[skiftv1.CreateProjectRequest]) (*connect.Response[skiftv1.CreateProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.CreateProject(ctx, orgID, actor, domain.NewProject{
		RequestID: req.Msg.GetRequestId(),
		Title:     req.Msg.GetTitle(),
		Slug:      req.Msg.GetSlug(),
		Priority:  priorities.str(req.Msg.GetPriority()),
		VCSURL:    req.Msg.GetVcsUrl(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.CreateProjectResponse{Project: toProject(*project)}), nil
}

// GetProject implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) GetProject(ctx context.Context, req *connect.Request[skiftv1.GetProjectRequest]) (*connect.Response[skiftv1.GetProjectResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.GetProject(ctx, orgID, req.Msg.GetProject())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.GetProjectResponse{Project: toProject(*project)}), nil
}

// ListProjects implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) ListProjects(ctx context.Context, req *connect.Request[skiftv1.ListProjectsRequest]) (*connect.Response[skiftv1.ListProjectsResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	offset, err := decodePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	projects, err := g.svc.ListProjects(ctx, orgID, domain.ProjectListQuery{
		IncludeArchived: req.Msg.GetIncludeArchived(),
		Limit:           size,
		Offset:          offset,
	})
	if err != nil {
		return nil, connectError(err)
	}
	res := &skiftv1.ListProjectsResponse{NextPageToken: nextPageToken(offset, size, len(projects))}
	for _, p := range projects {
		res.Projects = append(res.Projects, toProject(p))
	}
	return connect.NewResponse(res), nil
}

// UpdateProject implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) UpdateProject(ctx context.Context, req *connect.Request[skiftv1.UpdateProjectRequest]) (*connect.Response[skiftv1.UpdateProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.UpdateProject(ctx, orgID, actor, domain.UpdateProject{
		RequestID: req.Msg.GetRequestId(),
		Project:   req.Msg.GetProject(),
		VCSURL:    req.Msg.VcsUrl,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.UpdateProjectResponse{Project: toProject(*project)}), nil
}

// ArchiveProject implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) ArchiveProject(ctx context.Context, req *connect.Request[skiftv1.ArchiveProjectRequest]) (*connect.Response[skiftv1.ArchiveProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.ArchiveProject(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetProject())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.ArchiveProjectResponse{Project: toProject(*project)}), nil
}

// HoldProject implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) HoldProject(ctx context.Context, req *connect.Request[skiftv1.HoldProjectRequest]) (*connect.Response[skiftv1.HoldProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, held, err := g.svc.HoldProject(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetProject(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.HoldProjectResponse{
		Project: toProject(*project), HeldCount: int32(held), //nolint:gosec // a project's open nodes
	}), nil
}

// UnholdProject implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) UnholdProject(ctx context.Context, req *connect.Request[skiftv1.UnholdProjectRequest]) (*connect.Response[skiftv1.UnholdProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.UnholdProject(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetProject(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.UnholdProjectResponse{Project: toProject(*project)}), nil
}

// Focus implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) Focus(ctx context.Context, req *connect.Request[skiftv1.FocusRequest]) (*connect.Response[skiftv1.FocusResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	result, err := g.svc.Focus(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetProject(), req.Msg.GetKeep(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.FocusResponse{
		Group:         result.Group,
		HeldCount:     int32(result.Held),     //nolint:gosec // bounded by the organization's projects or a project's nodes
		ReleasedCount: int32(result.Released), //nolint:gosec // same
	}), nil
}

// ClearFocus implements skiftv1connect.GraphServiceHandler.
func (g *GraphService) ClearFocus(ctx context.Context, req *connect.Request[skiftv1.ClearFocusRequest]) (*connect.Response[skiftv1.ClearFocusResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	released, err := g.svc.ClearFocus(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetProject(), req.Msg.GetGroup(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&skiftv1.ClearFocusResponse{ReleasedCount: int32(released)}), nil //nolint:gosec // bounded as above
}
