package server

import (
	"context"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/domain"
)

// GraphService is the dagger.v1.GraphService handler. It translates each
// request into a domain call and the domain's answer into a response, and
// decides nothing about the graph itself. The actor of every call is the
// principal the transport resolved; nothing in a request names one.
type GraphService struct {
	svc *domain.Service
}

// The handler types implement the generated interfaces outright, without
// embedding the unimplemented defaults, so a proto RPC with no handler is a
// compile error rather than a runtime Unimplemented.
var _ daggerv1connect.GraphServiceHandler = (*GraphService)(nil)

// NewGraphService returns the GraphService handler over svc.
func NewGraphService(svc *domain.Service) *GraphService {
	return &GraphService{svc: svc}
}

// ApplyPatch implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) ApplyPatch(ctx context.Context, req *connect.Request[daggerv1.ApplyPatchRequest]) (*connect.Response[daggerv1.ApplyPatchResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	out, err := g.svc.Apply(ctx, orgID, actor, fromPatch(req.Msg))
	if err != nil {
		return nil, connectError(err)
	}
	view := viewOr(req.Msg.GetView(), daggerv1.View_VIEW_REFS)
	res := &daggerv1.ApplyPatchResponse{
		Nodes:      toNodes(out.States, view),
		Aliases:    out.Aliases,
		EventCount: int32(len(out.Events)), //nolint:gosec // bounded by the patch's operation ceiling
		RequestId:  req.Msg.GetRequestId(),
	}
	// Events are the audit record, read through WatchEvents; a patch
	// response carries them only when asked for everything.
	if view == daggerv1.View_VIEW_FULL {
		if res.Events, err = toEvents(out.Events); err != nil {
			return nil, connectError(err)
		}
	}
	return connect.NewResponse(res), nil
}

// GetNode implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) GetNode(ctx context.Context, req *connect.Request[daggerv1.GetNodeRequest]) (*connect.Response[daggerv1.GetNodeResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	node, err := g.svc.GetNode(ctx, orgID, req.Msg.GetProject(), req.Msg.GetNode())
	if err != nil {
		return nil, connectError(err)
	}
	view := viewOr(req.Msg.GetView(), daggerv1.View_VIEW_FULL)
	res := &daggerv1.GetNodeResponse{Node: projectNode(toNode(node.NodeState), view), Inputs: toInputsHint(node.Inputs)}
	for _, e := range node.Edges {
		res.Edges = append(res.Edges, toEdge(e))
	}
	if node.Lease != nil {
		res.Lease = toLease(*node.Lease)
	}
	// The result, artifacts, and comments are content, like the body:
	// only the full view carries them.
	if view != daggerv1.View_VIEW_FULL {
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
	return connect.NewResponse(res), nil
}

// GetInputs implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) GetInputs(ctx context.Context, req *connect.Request[daggerv1.GetInputsRequest]) (*connect.Response[daggerv1.GetInputsResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	inputs, err := g.svc.GetInputs(ctx, orgID, actor, req.Msg.GetProject(), req.Msg.GetNode())
	if err != nil {
		return nil, connectError(err)
	}
	view := viewOr(req.Msg.GetView(), daggerv1.View_VIEW_FULL)
	res := &daggerv1.GetInputsResponse{Inputs: make([]*daggerv1.Input, 0, len(inputs))}
	for _, in := range inputs {
		wire := &daggerv1.Input{Node: projectNode(toNode(in.NodeState), view), Bytes: resultBytes(in.Result)}
		// The result is content, like a body: a narrower view says how
		// big it is and leaves fetching it to the caller.
		if view == daggerv1.View_VIEW_FULL {
			if wire.Result, err = toResult(in.Result, in.Artifacts); err != nil {
				return nil, connectError(err)
			}
		}
		res.Inputs = append(res.Inputs, wire)
	}
	return connect.NewResponse(res), nil
}

// ListNodes implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) ListNodes(ctx context.Context, req *connect.Request[daggerv1.ListNodesRequest]) (*connect.Response[daggerv1.ListNodesResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	offset, err := decodePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	nodes, err := g.svc.ListNodes(ctx, orgID, domain.NodeListQuery{
		Project:      req.Msg.GetProject(),
		Status:       statuses.str(req.Msg.GetStatus()),
		Kind:         req.Msg.GetKind(),
		AssigneeKind: assigneeKinds.str(req.Msg.GetAssigneeKind()),
		Labels:       req.Msg.GetLabels(),
		Parent:       req.Msg.GetParent(),
		Limit:        size,
		Offset:       offset,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.ListNodesResponse{
		Nodes:         toNodes(nodes, viewOr(req.Msg.GetView(), daggerv1.View_VIEW_SUMMARY)),
		NextPageToken: nextPageToken(offset, size, len(nodes)),
	}), nil
}

// Explain implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) Explain(ctx context.Context, req *connect.Request[daggerv1.ExplainRequest]) (*connect.Response[daggerv1.ExplainResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	explained, err := g.svc.Explain(ctx, orgID, req.Msg.GetProject(), req.Msg.GetNode())
	if err != nil {
		return nil, connectError(err)
	}
	res := &daggerv1.ExplainResponse{Node: toNode(domain.NodeState{Node: explained.Node, Derived: explained.Derived[explained.Node.ID]})}
	for _, r := range explained.Reasons {
		res.Reasons = append(res.Reasons, toReason(r, explained.Derived))
	}
	return connect.NewResponse(res), nil
}

// Search implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) Search(ctx context.Context, req *connect.Request[daggerv1.SearchRequest]) (*connect.Response[daggerv1.SearchResponse], error) {
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
	res := &daggerv1.SearchResponse{}
	view := viewOr(req.Msg.GetView(), daggerv1.View_VIEW_SUMMARY)
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

// CreateProject implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) CreateProject(ctx context.Context, req *connect.Request[daggerv1.CreateProjectRequest]) (*connect.Response[daggerv1.CreateProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.CreateProject(ctx, orgID, actor, domain.NewProject{
		RequestID: req.Msg.GetRequestId(),
		Title:     req.Msg.GetTitle(),
		Slug:      req.Msg.GetSlug(),
		Priority:  priorities.str(req.Msg.GetPriority()),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.CreateProjectResponse{Project: toProject(*project)}), nil
}

// GetProject implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) GetProject(ctx context.Context, req *connect.Request[daggerv1.GetProjectRequest]) (*connect.Response[daggerv1.GetProjectResponse], error) {
	orgID, _, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.GetProject(ctx, orgID, req.Msg.GetProject())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.GetProjectResponse{Project: toProject(*project)}), nil
}

// ListProjects implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) ListProjects(ctx context.Context, req *connect.Request[daggerv1.ListProjectsRequest]) (*connect.Response[daggerv1.ListProjectsResponse], error) {
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
	res := &daggerv1.ListProjectsResponse{NextPageToken: nextPageToken(offset, size, len(projects))}
	for _, p := range projects {
		res.Projects = append(res.Projects, toProject(p))
	}
	return connect.NewResponse(res), nil
}

// ArchiveProject implements daggerv1connect.GraphServiceHandler.
func (g *GraphService) ArchiveProject(ctx context.Context, req *connect.Request[daggerv1.ArchiveProjectRequest]) (*connect.Response[daggerv1.ArchiveProjectResponse], error) {
	orgID, actor, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	project, err := g.svc.ArchiveProject(ctx, orgID, actor, req.Msg.GetRequestId(), req.Msg.GetProject())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&daggerv1.ArchiveProjectResponse{Project: toProject(*project)}), nil
}
