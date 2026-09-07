package server_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/server/interceptor"
	"github.com/ivy/dagger/internal/testsupport"
)

// loopbackTailnet stands in for the tsnet transport: a TCP listener on the
// loopback interface whose connections are marked exactly as the tailnet's
// are, and a WhoIs that names one node for every caller, as tsnet does for
// a single laptop. Everything downstream of the listener is the production
// path.
type loopbackTailnet struct {
	node     string
	listener net.Listener
	closed   bool
}

func (t *loopbackTailnet) Name() string { return "loopback-tailnet" }

func (t *loopbackTailnet) Listen() (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.listener = l
	return l, nil
}

func (t *loopbackTailnet) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	return interceptor.TailnetConnContext(ctx, conn)
}

func (t *loopbackTailnet) Close() error {
	t.closed = true
	return nil
}

func (t *loopbackTailnet) WhoIs(context.Context, string) (string, error) {
	return t.node, nil
}

// graph is a client reaching the handlers over the tailnet rather than the
// socket.
func (t *loopbackTailnet) graph() daggerv1connect.GraphServiceClient {
	return daggerv1connect.NewGraphServiceClient(&http.Client{}, "http://"+t.listener.Addr().String())
}

// failingTransport cannot bind.
type failingTransport struct{}

func (failingTransport) Name() string                  { return "broken" }
func (failingTransport) Listen() (net.Listener, error) { return nil, errors.New("no network") }
func (failingTransport) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return ctx
}
func (failingTransport) Close() error { return nil }

var _ = Describe("Connect server over the tailnet", func() {
	Describe("beside the socket", Label("unit"), func() {
		It("The same handler set is served over both transports", func(ctx SpecContext) {
			By("given a server on a socket and a tailnet with one handler set")
			tn := &loopbackTailnet{node: "nLAPTOP01"}
			r := start(server.Config{Transports: []server.Transport{tn}}, server.Services{Graph: graphStub{}}, pinger{})

			By("when the same call is made over each")
			viaSocket, err := r.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))
			Expect(err).NotTo(HaveOccurred())
			viaTailnet, err := tn.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))
			Expect(err).NotTo(HaveOccurred())

			By("then the one handler answers both")
			Expect(viaTailnet.Msg.GetProject().GetSlug()).To(Equal(viaSocket.Msg.GetProject().GetSlug()))
			Expect(viaTailnet.Msg.GetProject().GetTitle()).To(Equal("Stub project"))

			By("and the unmounted services answer Unimplemented over the tailnet too, not 404")
			_, err = daggerv1connect.NewWorkServiceClient(&http.Client{}, "http://"+tn.listener.Addr().String()).
				Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))

			By("and the transport is closed when the server stops")
			Expect(r.stop()).To(Succeed())
			Expect(tn.closed).To(BeTrue())
		})

		It("A tailnet caller with no mapped identity is rejected", func(ctx SpecContext) {
			By("when an unmapped tailnet node calls the service")
			tn := &loopbackTailnet{node: "nSTRANGER"}
			start(server.Config{
				Transports: []server.Transport{tn},
				Interceptors: []connect.Interceptor{interceptor.NewPrincipal(
					interceptor.StaticLookup{}, interceptor.UnixResolver{}, interceptor.TailscaleResolver{WhoIs: tn},
				)},
			}, server.Services{Graph: graphStub{}}, pinger{})
			_, err := tn.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))

			By("then the call fails as unauthenticated")
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("unwinds the socket when a transport cannot bind", func() {
			socket := GinkgoT().TempDir() + "/dg.sock"
			srv := server.New(server.Config{
				SocketPath: socket,
				Logger:     specLogger(),
				Transports: []server.Transport{failingTransport{}},
			}, server.Services{}, pinger{})
			Expect(srv.Listen()).To(MatchError(ContainSubstring("broken: no network")))
			_, err := net.Dial("unix", socket)
			Expect(err).To(HaveOccurred(), "the socket is not left listening")
		})
	})

	Describe("over the real domain", Label(testsupport.DBLabel), func() {
		It("The same person from two machines is one principal", func(ctx SpecContext) {
			By("given a principal with a unix identity and a tailscale identity")
			pool := pg.Database(ctx)
			org := newID()
			ivy := interceptor.Principal{ID: newID(), OrgID: org, Kind: interceptor.KindUser}
			_, err := pool.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ($1, $2)`, org, "acme")
			Expect(err).NotTo(HaveOccurred())
			_, err = pool.Exec(ctx, `INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, $3)`, ivy.ID, ivy.OrgID, string(ivy.Kind))
			Expect(err).NotTo(HaveOccurred())
			for _, id := range []interceptor.Identity{
				{Scheme: interceptor.SchemeUnix, Value: strconv.Itoa(os.Getuid())},
				{Scheme: interceptor.SchemeTailscale, Value: "nLAPTOP01"},
			} {
				_, err = pool.Exec(ctx, `INSERT INTO principal_identities (principal_id, scheme, value) VALUES ($1, $2, $3)`, ivy.ID, id.Scheme, id.Value)
				Expect(err).NotTo(HaveOccurred())
			}

			svc := domain.NewService(pool, pol)
			tn := &loopbackTailnet{node: "nLAPTOP01"}
			r := start(server.Config{
				Transports: []server.Transport{tn},
				Interceptors: []connect.Interceptor{interceptor.NewPrincipal(
					server.NewIdentityLookup(svc), interceptor.UnixResolver{}, interceptor.TailscaleResolver{WhoIs: tn},
				)},
			}, server.Services{Graph: server.NewGraphService(svc), Work: server.NewWorkService(svc)}, pool)

			By("when it calls over the socket and then over the tailnet")
			_, err = r.graph().CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{RequestId: newID(), Title: "Platform"}))
			Expect(err).NotTo(HaveOccurred())
			viaSocket, err := r.graph().ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
				RequestId: newID(), Project: "platform", Ops: []*daggerv1.PatchOp{createOp("a", "From the host")}, View: daggerv1.View_VIEW_FULL,
			}))
			Expect(err).NotTo(HaveOccurred())
			viaTailnet, err := tn.graph().ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
				RequestId: newID(), Project: "platform", Ops: []*daggerv1.PatchOp{createOp("b", "From the laptop")}, View: daggerv1.View_VIEW_FULL,
			}))
			Expect(err).NotTo(HaveOccurred())

			By("then both calls attribute to the same principal")
			Expect(viaSocket.Msg.GetEvents()).To(HaveLen(1))
			Expect(viaTailnet.Msg.GetEvents()).To(HaveLen(1))
			socketEvent, tailnetEvent := viaSocket.Msg.GetEvents()[0], viaTailnet.Msg.GetEvents()[0]
			Expect(socketEvent.GetPrincipal()).To(Equal(ivy.ID))
			Expect(tailnetEvent.GetPrincipal()).To(Equal(ivy.ID))

			By("and the events record different identity schemes")
			Expect(socketEvent.GetIdentityScheme()).To(Equal(interceptor.SchemeUnix))
			Expect(tailnetEvent.GetIdentityScheme()).To(Equal(interceptor.SchemeTailscale))

			var schemes []string
			rows, err := pool.Query(ctx, `SELECT identity_scheme FROM events WHERE principal_id = $1 AND op = 'create_node' ORDER BY id`, ivy.ID)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()
			for rows.Next() {
				var s string
				Expect(rows.Scan(&s)).To(Succeed())
				schemes = append(schemes, s)
			}
			Expect(schemes).To(Equal([]string{interceptor.SchemeUnix, interceptor.SchemeTailscale}))
		})
	})
})
