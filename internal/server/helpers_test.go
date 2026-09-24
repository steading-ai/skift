package server_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os/user"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/codec"
	"github.com/steading-ai/skift/internal/server"
)

// wireJSON renders a response the way a JSON client, and so a context
// window, receives it: the codec the handlers are mounted with.
func wireJSON(msg proto.Message) string {
	GinkgoHelper()
	raw, err := codec.Marshal(msg)
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}

// pinger is a database stand-in for the unit specs.
type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

// graphStub answers GetProject with a fixed project. It is what the
// handler node will replace; here it proves the transport carries a
// successful response, not only an error.
type graphStub struct {
	skiftv1connect.UnimplementedGraphServiceHandler
}

func (graphStub) GetProject(_ context.Context, req *connect.Request[skiftv1.GetProjectRequest]) (*connect.Response[skiftv1.GetProjectResponse], error) {
	return connect.NewResponse(&skiftv1.GetProjectResponse{
		Project: &skiftv1.Project{Slug: req.Msg.GetProject(), Title: "Stub project"},
	}), nil
}

// workStub waits in ClaimNext the way the lease implementation will: until
// work appears, the wait elapses, or the server drains. Nothing ever
// appears, so a call returns only on wait or drain. claims counts leases it
// would have handed out, and must stay zero across a drain.
type workStub struct {
	skiftv1connect.UnimplementedWorkServiceHandler
	waiting chan struct{}
	claims  atomic.Int32
}

func newWorkStub() *workStub {
	return &workStub{waiting: make(chan struct{}, 1)}
}

func (w *workStub) ClaimNext(ctx context.Context, req *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
	w.waiting <- struct{}{}
	wait := req.Msg.GetWait().AsDuration()
	select {
	case <-time.After(wait):
	case <-server.Draining(ctx):
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	}
	// A lease is created only when a node is handed out; an empty
	// response carries neither.
	return connect.NewResponse(&skiftv1.ClaimNextResponse{}), nil
}

// running is a server started for one spec.
type running struct {
	server *server.Server
	socket string
	cancel context.CancelFunc
	// stopped closes once Serve has returned err.
	stopped chan struct{}
	err     error
}

// start listens on a socket in a scratch directory and serves until the
// spec ends or stop is called. It is the in-process shape of "skift serve":
// the same Server, the same drain path, with services injected.
func start(cfg server.Config, svc server.Services, db server.Pinger) *running {
	GinkgoHelper()
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(GinkgoT().TempDir(), "skift.sock")
	}
	if cfg.Logger == nil {
		cfg.Logger = specLogger()
	}
	srv := server.New(cfg, svc, db)
	Expect(srv.Listen()).To(Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	r := &running{server: srv, socket: cfg.SocketPath, cancel: cancel, stopped: make(chan struct{})}
	go func() {
		defer GinkgoRecover()
		defer close(r.stopped)
		r.err = srv.Serve(ctx)
	}()
	DeferCleanup(r.stop)
	return r
}

// stop cancels the serve context, as SIGTERM does in skift serve, and returns
// what Serve returned. It is safe to call more than once.
func (r *running) stop() error {
	r.cancel()
	select {
	case <-r.stopped:
		return r.err
	case <-time.After(10 * time.Second):
		Fail("server did not stop within 10s")
		return errors.New("unreachable")
	}
}

// httpClient dials every request through the socket. The URL host is a
// placeholder that never resolves.
func (r *running) httpClient() *http.Client {
	socket := r.socket
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

const baseURL = "http://skift"

func (r *running) graph() skiftv1connect.GraphServiceClient {
	return skiftv1connect.NewGraphServiceClient(r.httpClient(), baseURL)
}

func (r *running) work() skiftv1connect.WorkServiceClient {
	return skiftv1connect.NewWorkServiceClient(r.httpClient(), baseURL)
}

func (r *running) events() skiftv1connect.EventServiceClient {
	return skiftv1connect.NewEventServiceClient(r.httpClient(), baseURL)
}

// specLogger routes the server's log to the spec's writer, shown only on
// failure or under -v.
func specLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// groupName resolves a numeric gid to its name.
func groupName(gid int) string {
	GinkgoHelper()
	g, err := user.LookupGroupId(strconv.Itoa(gid))
	Expect(err).NotTo(HaveOccurred())
	return g.Name
}
