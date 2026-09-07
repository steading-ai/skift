package cli_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/server"
)

// The CLI specs run the commands against an in-memory stand-in for the
// service, mounted on the real transport: the same server package `dg
// serve` uses, on a unix socket in a scratch directory. The stub keeps
// just enough state to answer the way the handlers do, and records every
// request so a spec can check what a command sent.

// expiredLease is the lease id the stub treats as no longer live.
const expiredLease = "01JEXPIRED00000000000000000"

type stub struct {
	daggerv1connect.UnimplementedEventServiceHandler

	mu       sync.Mutex
	nodes    []*daggerv1.Node
	projects map[string]*daggerv1.Project
	// projectOrder is the slugs in creation order, so ListProjects can
	// answer most recently active first as the handler does.
	projectOrder []string
	// leases are every lease handed out, by id. A lease expires the way
	// the server's do: a claim past its expiry returns the node to the
	// ready set.
	leases map[string]*stubLease
	// requests are every request message received, in order.
	requests []any
	// readyGate, when set, is closed by a spec to make work appear for a
	// waiting ClaimNext.
	readyGate chan struct{}
	// refsOnly makes ApplyPatch answer in the refs view, as the service
	// does when a request asks for no more, instead of echoing the node.
	refsOnly bool
}

// stubLease is a lease the stub handed out and its state.
type stubLease struct {
	node      *daggerv1.Node
	expiresAt time.Time
	reason    daggerv1.ReleaseReason
}

func newStub() *stub {
	return &stub{projects: map[string]*daggerv1.Project{}, leases: map[string]*stubLease{}}
}

func (s *stub) record(req any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
}

// last returns the most recent request of type T.
func last[T any](s *stub) T {
	GinkgoHelper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if req, ok := s.requests[i].(T); ok {
			return req
		}
	}
	var zero T
	Fail(fmt.Sprintf("no %T was received", zero))
	return zero
}

// count returns how many requests of type T were received.
func count[T any](s *stub) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, req := range s.requests {
		if _, ok := req.(T); ok {
			n++
		}
	}
	return n
}

func (s *stub) newNode(title string) *daggerv1.Node {
	n := &daggerv1.Node{
		Id:        fmt.Sprintf("01J%023d", len(s.nodes)+1),
		Number:    int64(len(s.nodes) + 1),
		Reference: fmt.Sprintf("platform#%d", len(s.nodes)+1),
		Kind:      "task",
		Title:     title,
		Status:    daggerv1.Status_STATUS_OPEN,
		Priority:  daggerv1.Priority_PRIORITY_P2,
		CreatedAt: timestamppb.Now(),
		Ready:     true,
	}
	s.nodes = append(s.nodes, n)
	return n
}

// newProject records a project the way CreateProject does, so a spec can
// give the stub a backlog of projects to list. The most recently created
// is the most recently active.
func (s *stub) newProject(slug, title string) *daggerv1.Project {
	p := &daggerv1.Project{
		Id:          fmt.Sprintf("01JPROJECT%016d", len(s.projects)+1),
		Slug:        slug,
		Title:       title,
		Priority:    daggerv1.Priority_PRIORITY_P2,
		NodeCounter: int64(len(s.nodes) + 1),
		CreatedAt:   timestamppb.Now(),
	}
	if _, ok := s.projects[slug]; !ok {
		s.projectOrder = append(s.projectOrder, slug)
	}
	s.projects[slug] = p
	return p
}

// lease renders a lease. An id the stub never handed out is rendered live,
// so specs can name leases without claiming first.
func (s *stub) lease(id string) *daggerv1.Lease {
	l := &daggerv1.Lease{Id: id, Holder: "01JAGENT000000000000000000", Harness: "dg", ExpiresAt: timestamppb.New(time.Now().Add(15 * time.Minute))}
	if held, ok := s.leases[id]; ok {
		l.NodeId = held.node.GetId()
		l.ExpiresAt = timestamppb.New(held.expiresAt)
		l.ReleaseReason = held.reason
	}
	return l
}

// expire releases every lease past its expiry, returning its node to the
// ready set, as the server's reaper does. Callers hold s.mu.
func (s *stub) expire() {
	for _, l := range s.leases {
		if l.reason == daggerv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED && time.Now().After(l.expiresAt) {
			l.reason = daggerv1.ReleaseReason_RELEASE_REASON_EXPIRED
			l.node.Claimed, l.node.Ready = false, true
		}
	}
}

// ttlOr is the requested TTL, or the default.
func ttlOr(d *durationpb.Duration) time.Duration {
	if d == nil || d.AsDuration() <= 0 {
		return 15 * time.Minute
	}
	return d.AsDuration()
}

// GraphService.

func (s *stub) ApplyPatch(_ context.Context, req *connect.Request[daggerv1.ApplyPatchRequest]) (*connect.Response[daggerv1.ApplyPatchResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetRequestId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, domain.ErrNoRequestID)
	}
	if req.Msg.GetProject() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, domain.ErrProjectRequired)
	}
	res := &daggerv1.ApplyPatchResponse{Aliases: map[string]string{}, RequestId: req.Msg.GetRequestId()}
	for _, op := range req.Msg.GetOps() {
		switch o := op.GetOp().(type) {
		case *daggerv1.PatchOp_CreateNode:
			n := s.newNode(o.CreateNode.GetTitle())
			n.Body = o.CreateNode.GetBody()
			n.Labels = o.CreateNode.GetLabels()
			if k := o.CreateNode.GetKind(); k != "" {
				n.Kind = k
			}
			if p := o.CreateNode.GetPriority(); p != daggerv1.Priority_PRIORITY_UNSPECIFIED {
				n.Priority = p
			}
			n.Blocked = len(o.CreateNode.GetBlockedBy()) > 0
			n.Ready = !n.Blocked
			res.EventCount++
			if s.refsOnly {
				res.Nodes = append(res.Nodes, &daggerv1.Node{Number: n.GetNumber(), Reference: n.GetReference(), Status: n.GetStatus(), Ready: n.GetReady(), Blocked: n.GetBlocked()})
			} else {
				res.Nodes = append(res.Nodes, n)
			}
			if alias := o.CreateNode.GetAlias(); alias != "" {
				res.Aliases[alias] = n.GetReference()
			}
		case *daggerv1.PatchOp_CompleteNode:
			if req.Msg.GetLeaseId() == expiredLease {
				return nil, connect.NewError(connect.CodeFailedPrecondition, domain.ErrLeaseNotLive)
			}
			if len(s.nodes) > 0 {
				n := s.nodes[0]
				n.Status = daggerv1.Status_STATUS_DONE
				n.Ready = false
				res.EventCount++
				if s.refsOnly {
					res.Nodes = append(res.Nodes, &daggerv1.Node{Number: n.GetNumber(), Reference: n.GetReference(), Status: n.GetStatus()})
				} else {
					res.Nodes = append(res.Nodes, n)
				}
			}
		case *daggerv1.PatchOp_UpdateNode:
			if len(s.nodes) > 0 {
				n := s.nodes[0]
				if o.UpdateNode.Title != nil {
					n.Title = o.UpdateNode.GetTitle()
				}
				if o.UpdateNode.Body != nil {
					n.Body = o.UpdateNode.GetBody()
				}
				if o.UpdateNode.Kind != nil {
					n.Kind = o.UpdateNode.GetKind()
				}
				if o.UpdateNode.Priority != nil {
					n.Priority = o.UpdateNode.GetPriority()
				}
				if o.UpdateNode.Labels != nil {
					n.Labels = o.UpdateNode.GetLabels().GetValues()
				}
				res.EventCount++
				if s.refsOnly {
					res.Nodes = append(res.Nodes, &daggerv1.Node{Number: n.GetNumber(), Reference: n.GetReference(), Status: n.GetStatus(), Ready: n.GetReady(), Blocked: n.GetBlocked()})
				} else {
					res.Nodes = append(res.Nodes, n)
				}
			}
		case *daggerv1.PatchOp_DropNode:
			if len(s.nodes) > 0 {
				n := s.nodes[0]
				n.Status = daggerv1.Status_STATUS_DROPPED
				n.Ready = false
				res.EventCount++
				res.Dropped = append(res.Dropped, n.GetReference())
				res.Nodes = append(res.Nodes, &daggerv1.Node{Number: n.GetNumber(), Reference: n.GetReference(), Status: n.GetStatus()})
			}
		case *daggerv1.PatchOp_AddEdge, *daggerv1.PatchOp_AddComment, *daggerv1.PatchOp_EstimateNode:
			// Recorded; nothing to return.
		default:
			return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("stub: %T", o))
		}
	}
	return connect.NewResponse(res), nil
}

func (s *stub) GetNode(_ context.Context, req *connect.Request[daggerv1.GetNodeRequest]) (*connect.Response[daggerv1.GetNodeResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nodes) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, domain.ErrNodeNotFound)
	}
	res := &daggerv1.GetNodeResponse{
		Node:   s.nodes[0],
		Inputs: &daggerv1.InputsHint{Count: int32(len(s.nodes) - 1), Bytes: 42}, //nolint:gosec // a spec's node count
	}
	// The full view is the one that carries content, and cost is content:
	// the node's current estimate and the rollup over its subtree.
	if req.Msg.GetView() == daggerv1.View_VIEW_FULL || req.Msg.GetView() == daggerv1.View_VIEW_UNSPECIFIED {
		res.Estimate = &daggerv1.Usage{
			Kind: daggerv1.UsageKind_USAGE_KIND_ESTIMATE, Model: "claude-opus-5",
			InputTokens: 120000, OutputTokens: 8000, Basis: "two nodes of the same shape",
		}
		res.Usage = &daggerv1.Rollup{
			Actual:   []*daggerv1.ModelTotals{{Model: "claude-opus-5", Records: 2, InputTokens: 2000, OutputTokens: 400}},
			Estimate: []*daggerv1.ModelTotals{{Model: "claude-opus-5", Records: 1, InputTokens: 120000, OutputTokens: 8000}},
		}
	}
	return connect.NewResponse(res), nil
}

// GetInputs answers with every node but the last as an input of the last,
// each carrying a result, which is enough for a spec to check what a
// command sent and printed.
func (s *stub) GetInputs(_ context.Context, req *connect.Request[daggerv1.GetInputsRequest]) (*connect.Response[daggerv1.GetInputsResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nodes) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, domain.ErrNodeNotFound)
	}
	res := &daggerv1.GetInputsResponse{}
	for _, n := range s.nodes[:len(s.nodes)-1] {
		body := "result of " + n.GetTitle()
		in := &daggerv1.Input{Node: n, Bytes: int64(len(body))}
		if req.Msg.GetView() != daggerv1.View_VIEW_SUMMARY && req.Msg.GetView() != daggerv1.View_VIEW_REFS {
			in.Result = &daggerv1.Result{Body: body}
		}
		res.Inputs = append(res.Inputs, in)
	}
	return connect.NewResponse(res), nil
}

// ListNodes filters the stub's nodes the way the handler does — status,
// kind, labels and assignee kind narrow the page, never widen it — and
// projects each through the requested view, which defaults to a summary.
func (s *stub) ListNodes(_ context.Context, req *connect.Request[daggerv1.ListNodesRequest]) (*connect.Response[daggerv1.ListNodesResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &daggerv1.ListNodesResponse{}
	for _, n := range s.nodes {
		if !matches(n, req.Msg) {
			continue
		}
		res.Nodes = append(res.Nodes, summarize(n, req.Msg.GetView()))
	}
	return connect.NewResponse(res), nil
}

// matches reports whether a node passes every filter the request sets.
func matches(n *daggerv1.Node, req *daggerv1.ListNodesRequest) bool {
	if st := req.GetStatus(); st != daggerv1.Status_STATUS_UNSPECIFIED && n.GetStatus() != st {
		return false
	}
	if k := req.GetKind(); k != "" && n.GetKind() != k {
		return false
	}
	if ak := req.GetAssigneeKind(); ak != daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED && n.GetAssigneeKind() != ak {
		return false
	}
	for _, want := range req.GetLabels() {
		if !slices.Contains(n.GetLabels(), want) {
			return false
		}
	}
	return true
}

// summarize is the stub's stand-in for the server's view projection: only
// VIEW_FULL carries the body.
func summarize(n *daggerv1.Node, view daggerv1.View) *daggerv1.Node {
	if view == daggerv1.View_VIEW_FULL {
		return n
	}
	out := proto.Clone(n).(*daggerv1.Node) //nolint:forcetypeassert // Clone returns the same type
	out.Body = ""
	return out
}

func (s *stub) Explain(_ context.Context, req *connect.Request[daggerv1.ExplainRequest]) (*connect.Response[daggerv1.ExplainResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nodes) < 2 {
		return nil, connect.NewError(connect.CodeNotFound, domain.ErrNodeNotFound)
	}
	return connect.NewResponse(&daggerv1.ExplainResponse{
		Node:    s.nodes[1],
		Reasons: []*daggerv1.Reason{{Reason: &daggerv1.Reason_Blocked{Blocked: &daggerv1.BlockedReason{Blocker: s.nodes[0]}}}},
	}), nil
}

func (s *stub) Search(_ context.Context, req *connect.Request[daggerv1.SearchRequest]) (*connect.Response[daggerv1.SearchResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &daggerv1.SearchResponse{}
	for _, n := range s.nodes {
		res.Hits = append(res.Hits, &daggerv1.SearchHit{Node: n, Snippet: "<b>" + req.Msg.GetQuery() + "</b>"})
	}
	return connect.NewResponse(res), nil
}

func (s *stub) CreateProject(_ context.Context, req *connect.Request[daggerv1.CreateProjectRequest]) (*connect.Response[daggerv1.CreateProjectResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	slug := req.Msg.GetSlug()
	if slug == "" {
		slug = "derived"
	}
	p := s.newProject(slug, req.Msg.GetTitle())
	p.Priority = req.Msg.GetPriority()
	return connect.NewResponse(&daggerv1.CreateProjectResponse{Project: p}), nil
}

func (s *stub) GetProject(_ context.Context, req *connect.Request[daggerv1.GetProjectRequest]) (*connect.Response[daggerv1.GetProjectResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[req.Msg.GetProject()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, &domain.NotFoundError{
			Ref: domain.Ref{Project: req.Msg.GetProject()}, Missing: domain.MissingProject,
		})
	}
	return connect.NewResponse(&daggerv1.GetProjectResponse{Project: p}), nil
}

func (s *stub) ListProjects(_ context.Context, req *connect.Request[daggerv1.ListProjectsRequest]) (*connect.Response[daggerv1.ListProjectsResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &daggerv1.ListProjectsResponse{NextPageToken: "next"}
	for i := len(s.projectOrder) - 1; i >= 0; i-- {
		p := s.projects[s.projectOrder[i]]
		if p.GetArchivedAt() != nil && !req.Msg.GetIncludeArchived() {
			continue
		}
		res.Projects = append(res.Projects, p)
	}
	return connect.NewResponse(res), nil
}

func (s *stub) ArchiveProject(_ context.Context, req *connect.Request[daggerv1.ArchiveProjectRequest]) (*connect.Response[daggerv1.ArchiveProjectResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[req.Msg.GetProject()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, domain.ErrProjectNotFound)
	}
	p.ArchivedAt = timestamppb.Now()
	return connect.NewResponse(&daggerv1.ArchiveProjectResponse{Project: p}), nil
}

// WorkService.

func (s *stub) ready() []*daggerv1.Node {
	s.expire()
	var out []*daggerv1.Node
	for _, n := range s.nodes {
		if n.GetReady() {
			out = append(out, n)
		}
	}
	return out
}

func (s *stub) Ready(_ context.Context, req *connect.Request[daggerv1.ReadyRequest]) (*connect.Response[daggerv1.ReadyResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetProject() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, domain.ErrProjectRequired)
	}
	return connect.NewResponse(&daggerv1.ReadyResponse{Nodes: s.ready()}), nil
}

// ClaimNext hands out the first ready node, or waits for one up to the
// request's wait, bounded like the real handler.
func (s *stub) ClaimNext(ctx context.Context, req *connect.Request[daggerv1.ClaimNextRequest]) (*connect.Response[daggerv1.ClaimNextResponse], error) {
	s.record(req.Msg)
	deadline := time.Now().Add(min(req.Msg.GetWait().AsDuration(), domain.MaxClaimWait))
	for {
		s.mu.Lock()
		ready := s.ready()
		gate := s.readyGate
		s.mu.Unlock()
		if len(ready) > 0 {
			n := ready[0]
			n.Ready, n.Claimed = false, true
			s.mu.Lock()
			id := fmt.Sprintf("01JLEASE%019d", len(s.leases)+1)
			s.leases[id] = &stubLease{node: n, expiresAt: time.Now().Add(ttlOr(req.Msg.GetTtl()))}
			l := s.lease(id)
			s.mu.Unlock()
			return connect.NewResponse(&daggerv1.ClaimNextResponse{Node: n, Lease: l}), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return connect.NewResponse(&daggerv1.ClaimNextResponse{}), nil
		}
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		case <-gate:
		case <-time.After(min(remaining, 20*time.Millisecond)):
		}
	}
}

func (s *stub) Heartbeat(_ context.Context, req *connect.Request[daggerv1.HeartbeatRequest]) (*connect.Response[daggerv1.HeartbeatResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if req.Msg.GetLeaseId() == expiredLease {
		return nil, connect.NewError(connect.CodeFailedPrecondition, domain.ErrLeaseNotLive)
	}
	if held, ok := s.leases[req.Msg.GetLeaseId()]; ok {
		if held.reason != daggerv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED {
			return nil, connect.NewError(connect.CodeFailedPrecondition, domain.ErrLeaseNotLive)
		}
		held.expiresAt = time.Now().Add(ttlOr(req.Msg.GetTtl()))
	}
	return connect.NewResponse(&daggerv1.HeartbeatResponse{Lease: s.lease(req.Msg.GetLeaseId())}), nil
}

func (s *stub) Release(_ context.Context, req *connect.Request[daggerv1.ReleaseRequest]) (*connect.Response[daggerv1.ReleaseResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, ok := s.leases[req.Msg.GetLeaseId()]; ok {
		held.reason = req.Msg.GetReason()
		held.node.Claimed, held.node.Ready = false, true
	}
	l := s.lease(req.Msg.GetLeaseId())
	l.ReleaseReason = req.Msg.GetReason()
	return connect.NewResponse(&daggerv1.ReleaseResponse{Lease: l}), nil
}

// ReportUsage records the report and answers with the record the service
// would have stored, so a spec can check what the command sent.
func (s *stub) ReportUsage(_ context.Context, req *connect.Request[daggerv1.ReportUsageRequest]) (*connect.Response[daggerv1.ReportUsageResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Msg.GetLeaseId() == expiredLease {
		return nil, connect.NewError(connect.CodeFailedPrecondition, domain.ErrLeaseNotLive)
	}
	return connect.NewResponse(&daggerv1.ReportUsageResponse{Usage: &daggerv1.Usage{
		LeaseId: req.Msg.GetLeaseId(), Model: req.Msg.GetModel(), Kind: daggerv1.UsageKind_USAGE_KIND_ACTUAL,
		InputTokens: req.Msg.GetInputTokens(), OutputTokens: req.Msg.GetOutputTokens(),
		CacheReadTokens: req.Msg.GetCacheReadTokens(), CacheWriteTokens: req.Msg.GetCacheWriteTokens(),
		CreatedAt: timestamppb.Now(),
	}}), nil
}

// EventService.

func (s *stub) WatchEvents(_ context.Context, req *connect.Request[daggerv1.WatchEventsRequest], stream *connect.ServerStream[daggerv1.WatchEventsResponse]) error {
	s.record(req.Msg)
	for i := 1; i <= 2; i++ {
		if err := stream.Send(&daggerv1.WatchEventsResponse{
			Event:  &daggerv1.Event{Id: fmt.Sprintf("01JEVENT%018d", i), Op: "create_node", OccurredAt: timestamppb.Now()},
			Cursor: fmt.Sprintf("c%d", i),
		}); err != nil {
			return err
		}
	}
	return nil
}

// serve mounts the stub on a socket in a scratch directory for the length
// of the spec and returns the socket path.
func serve(s *stub) string {
	GinkgoHelper()
	socket := filepath.Join(GinkgoT().TempDir(), "dg.sock")
	srv := server.New(server.Config{SocketPath: socket, ShutdownTimeout: 2 * time.Second, Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))},
		server.Services{Graph: s, Work: s, Events: s}, nil)
	Expect(srv.Listen()).To(Succeed())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(srv.Serve(ctx)).To(Succeed())
	}()
	DeferCleanup(func() {
		cancel()
		Eventually(stopped, 5*time.Second).Should(BeClosed())
	})
	return socket
}
