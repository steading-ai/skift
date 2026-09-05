package cli_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
	// requests are every request message received, in order.
	requests []any
	// readyGate, when set, is closed by a spec to make work appear for a
	// waiting ClaimNext.
	readyGate chan struct{}
}

func newStub() *stub {
	return &stub{projects: map[string]*daggerv1.Project{}}
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

func (s *stub) newNode(title string) *daggerv1.Node {
	n := &daggerv1.Node{
		Id:        fmt.Sprintf("01J%023d", len(s.nodes)+1),
		Number:    int64(len(s.nodes) + 1),
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

func (s *stub) lease(id string) *daggerv1.Lease {
	return &daggerv1.Lease{Id: id, Holder: "01JAGENT000000000000000000", Harness: "dg", ExpiresAt: timestamppb.New(time.Now().Add(15 * time.Minute))}
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
	res := &daggerv1.ApplyPatchResponse{Aliases: map[string]string{}}
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
			res.Nodes = append(res.Nodes, n)
			if alias := o.CreateNode.GetAlias(); alias != "" {
				res.Aliases[alias] = fmt.Sprintf("%s#%d", req.Msg.GetProject(), n.GetNumber())
			}
		case *daggerv1.PatchOp_CompleteNode:
			if req.Msg.GetLeaseId() == expiredLease {
				return nil, connect.NewError(connect.CodeFailedPrecondition, domain.ErrLeaseNotLive)
			}
			if len(s.nodes) > 0 {
				n := s.nodes[0]
				n.Status = daggerv1.Status_STATUS_DONE
				n.Ready = false
				res.Nodes = append(res.Nodes, n)
			}
		case *daggerv1.PatchOp_AddEdge, *daggerv1.PatchOp_AddComment:
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
	return connect.NewResponse(&daggerv1.GetNodeResponse{Node: s.nodes[0]}), nil
}

func (s *stub) ListNodes(_ context.Context, req *connect.Request[daggerv1.ListNodesRequest]) (*connect.Response[daggerv1.ListNodesResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&daggerv1.ListNodesResponse{Nodes: s.nodes}), nil
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
	p := &daggerv1.Project{Id: fmt.Sprintf("01JPROJECT%016d", len(s.projects)+1), Slug: slug, Title: req.Msg.GetTitle(), Priority: req.Msg.GetPriority(), CreatedAt: timestamppb.Now()}
	s.projects[slug] = p
	return connect.NewResponse(&daggerv1.CreateProjectResponse{Project: p}), nil
}

func (s *stub) GetProject(_ context.Context, req *connect.Request[daggerv1.GetProjectRequest]) (*connect.Response[daggerv1.GetProjectResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[req.Msg.GetProject()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, domain.ErrProjectNotFound)
	}
	return connect.NewResponse(&daggerv1.GetProjectResponse{Project: p}), nil
}

func (s *stub) ListProjects(_ context.Context, req *connect.Request[daggerv1.ListProjectsRequest]) (*connect.Response[daggerv1.ListProjectsResponse], error) {
	s.record(req.Msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &daggerv1.ListProjectsResponse{NextPageToken: "next"}
	for _, p := range s.projects {
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
			return connect.NewResponse(&daggerv1.ClaimNextResponse{Node: n, Lease: s.lease("01JLEASE0000000000000000001")}), nil
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
	if req.Msg.GetLeaseId() == expiredLease {
		return nil, connect.NewError(connect.CodeFailedPrecondition, domain.ErrLeaseNotLive)
	}
	return connect.NewResponse(&daggerv1.HeartbeatResponse{Lease: s.lease(req.Msg.GetLeaseId())}), nil
}

func (s *stub) Release(_ context.Context, req *connect.Request[daggerv1.ReleaseRequest]) (*connect.Response[daggerv1.ReleaseResponse], error) {
	s.record(req.Msg)
	l := s.lease(req.Msg.GetLeaseId())
	l.ReleaseReason = req.Msg.GetReason()
	return connect.NewResponse(&daggerv1.ReleaseResponse{Lease: l}), nil
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
