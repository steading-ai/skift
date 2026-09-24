package interceptor_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/server/interceptor"
	"github.com/ivy/dagger/internal/testsupport"
)

// attribution is what a handler saw in its context for one call.
type attribution struct {
	principal interceptor.Principal
	identity  interceptor.Identity
	ok        bool
}

// recordingGraph is a handler that reports who the context says called it.
// It reads nothing from the request body, which is the whole point: the
// same handler serves every transport and every spec below unchanged.
type recordingGraph struct {
	daggerv1connect.UnimplementedGraphServiceHandler
	seen chan attribution
}

func newRecordingGraph() *recordingGraph {
	return &recordingGraph{seen: make(chan attribution, 8)}
}

func (g *recordingGraph) GetProject(ctx context.Context, req *connect.Request[daggerv1.GetProjectRequest]) (*connect.Response[daggerv1.GetProjectResponse], error) {
	p, ok := interceptor.PrincipalFromContext(ctx)
	id, _ := interceptor.IdentityFromContext(ctx)
	g.seen <- attribution{principal: p, identity: id, ok: ok}
	return connect.NewResponse(&daggerv1.GetProjectResponse{
		Project: &daggerv1.Project{Slug: req.Msg.GetProject()},
	}), nil
}

// headerResolver is a stand-in for a second transport: it trusts a header.
// Nothing real would, but it shows a resolver being added beside the unix
// one without the handler knowing.
type headerResolver struct{}

func (headerResolver) Scheme() string { return "spec" }

func (headerResolver) Resolve(_ context.Context, _ connect.Peer, header http.Header) (interceptor.Identity, error) {
	v := header.Get("X-Spec-Identity")
	if v == "" {
		return interceptor.Identity{}, interceptor.ErrNoIdentity
	}
	return interceptor.Identity{Scheme: "spec", Value: v}, nil
}

var (
	alice = interceptor.Principal{ID: "01J0000000000000000000ALICE", OrgID: "01J00000000000000000000ORG", Kind: interceptor.KindAgent}
	// myUID is the uid this spec process runs as, and therefore the uid
	// SO_PEERCRED reports for every client it opens.
	myUID = os.Getuid()
	// myIdentity is the unix identity of this process.
	myIdentity = interceptor.Identity{Scheme: interceptor.SchemeUnix, Value: strconv.Itoa(myUID)}
)

// serve starts a server on a unix socket with the principal interceptor in
// front of the given services and returns a client factory for it.
func serve(lookup interceptor.Lookup, svc server.Services, resolvers ...interceptor.Resolver) *http.Client {
	GinkgoHelper()
	socket := filepath.Join(GinkgoT().TempDir(), "dg.sock")
	srv := server.New(server.Config{
		SocketPath:   socket,
		Logger:       slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolvers...)},
	}, svc, nil)
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
		Eventually(stopped).Should(BeClosed())
	})
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

const baseURL = "http://dagger"

var _ = Describe("Principal resolution interceptor", Label("unit"), func() {
	It("Identity comes from the socket", func(ctx SpecContext) {
		testsupport.RequirePeerCredentials()
		By("given a unix identity mapping this process's uid to a principal")
		lookup := interceptor.StaticLookup{myIdentity: alice}
		graph := newRecordingGraph()
		client := serve(lookup, server.Services{Graph: graph}, interceptor.UnixResolver{})

		By("when a process running as that uid calls the service")
		_, err := daggerv1connect.NewGraphServiceClient(client, baseURL).
			GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))
		Expect(err).NotTo(HaveOccurred())

		By("then the request is attributed to that principal")
		var seen attribution
		Expect(graph.seen).To(Receive(&seen))
		Expect(seen.ok).To(BeTrue(), "the handler found a principal in its context")
		Expect(seen.principal).To(Equal(alice))

		By("and the identity scheme used is recorded for the event")
		Expect(seen.identity).To(Equal(myIdentity))
		Expect(seen.identity.Scheme).To(Equal(interceptor.SchemeUnix))
	})

	It("An unknown caller is rejected", func(ctx SpecContext) {
		By("given a process whose uid maps to no identity")
		graph := newRecordingGraph()
		client := serve(interceptor.StaticLookup{}, server.Services{Graph: graph}, interceptor.UnixResolver{})

		By("when it calls any method")
		_, graphErr := daggerv1connect.NewGraphServiceClient(client, baseURL).
			GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))
		_, workErr := daggerv1connect.NewWorkServiceClient(client, baseURL).
			Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{}))
		stream, streamErr := daggerv1connect.NewEventServiceClient(client, baseURL).
			WatchEvents(ctx, connect.NewRequest(&daggerv1.WatchEventsRequest{}))
		Expect(streamErr).NotTo(HaveOccurred())
		stream.Receive()
		Expect(stream.Close()).To(Succeed())

		By("then the call fails as unauthenticated")
		Expect(connect.CodeOf(graphErr)).To(Equal(connect.CodeUnauthenticated))
		Expect(connect.CodeOf(workErr)).To(Equal(connect.CodeUnauthenticated))
		Expect(connect.CodeOf(stream.Err())).To(Equal(connect.CodeUnauthenticated), "streams are gated too")

		By("and no handler ran")
		Expect(graph.seen).NotTo(Receive())
	})

	It("maps a uid to a principal through the typed unmapped error", func(ctx SpecContext) {
		lookup := interceptor.StaticLookup{
			{Scheme: interceptor.SchemeUnix, Value: "1001"}: alice,
		}
		p, err := lookup.LookupIdentity(ctx, interceptor.Identity{Scheme: interceptor.SchemeUnix, Value: "1001"})
		Expect(err).NotTo(HaveOccurred())
		Expect(p).To(Equal(alice))

		_, err = lookup.LookupIdentity(ctx, interceptor.Identity{Scheme: interceptor.SchemeUnix, Value: "1002"})
		var unmapped *interceptor.UnmappedIdentityError
		Expect(err).To(BeAssignableToTypeOf(unmapped))
		Expect(err.Error()).To(ContainSubstring("unix:1002"))
	})

	It("resolves uid 1001 from peer credentials on the connection", func(ctx SpecContext) {
		By("given a connection whose peer the kernel reports as uid 1001")
		ctx2 := interceptor.WithPeerCredentials(ctx, interceptor.PeerCredentials{UID: 1001, GID: 1001, PID: 4242})

		By("when the unix resolver runs")
		id, err := interceptor.UnixResolver{}.Resolve(ctx2, connect.Peer{}, http.Header{})

		By("then the identity is the unix scheme with that uid")
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal(interceptor.Identity{Scheme: interceptor.SchemeUnix, Value: "1001"}))

		By("and a connection with no peer credentials is not this transport's")
		_, err = interceptor.UnixResolver{}.Resolve(ctx, connect.Peer{}, http.Header{})
		Expect(err).To(MatchError(interceptor.ErrNoIdentity))
	})

	It("Adding a resolver requires no handler change", func(ctx SpecContext) {
		testsupport.RequirePeerCredentials()
		By("given the unix resolver and a second resolver for another transport, in front of the same handler")
		bob := interceptor.Principal{ID: "01J00000000000000000000BOB", OrgID: alice.OrgID, Kind: interceptor.KindUser}
		lookup := interceptor.StaticLookup{
			myIdentity:                     alice,
			{Scheme: "spec", Value: "bob"}: bob,
		}
		graph := newRecordingGraph()

		By("when a caller arrives over a transport the unix resolver does not recognise")
		// A TCP server: no unix socket, so no peer credentials, so the
		// unix resolver declines and the next resolver decides.
		mux := http.NewServeMux()
		mux.Handle(daggerv1connect.NewGraphServiceHandler(graph, connect.WithInterceptors(
			interceptor.NewPrincipal(lookup, interceptor.UnixResolver{}, headerResolver{}),
		)))
		ts := httptest.NewServer(mux)
		DeferCleanup(ts.Close)
		req := connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"})
		req.Header().Set("X-Spec-Identity", "bob")
		_, err := daggerv1connect.NewGraphServiceClient(ts.Client(), ts.URL).GetProject(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		By("then the unchanged handler is attributed through the second scheme")
		var seen attribution
		Expect(graph.seen).To(Receive(&seen))
		Expect(seen.principal).To(Equal(bob))
		Expect(seen.identity.Scheme).To(Equal("spec"))

		By("and the same handler over the socket is attributed through the unix scheme")
		client := serve(lookup, server.Services{Graph: graph}, interceptor.UnixResolver{}, headerResolver{})
		_, err = daggerv1connect.NewGraphServiceClient(client, baseURL).
			GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(graph.seen).To(Receive(&seen))
		Expect(seen.principal).To(Equal(alice))
		Expect(seen.identity.Scheme).To(Equal(interceptor.SchemeUnix))
	})

	It("rejects a caller no resolver recognises rather than serving it anonymously", func(ctx SpecContext) {
		graph := newRecordingGraph()
		mux := http.NewServeMux()
		mux.Handle(daggerv1connect.NewGraphServiceHandler(graph, connect.WithInterceptors(
			interceptor.NewPrincipal(interceptor.StaticLookup{}, interceptor.UnixResolver{}),
		)))
		ts := httptest.NewServer(mux)
		DeferCleanup(ts.Close)
		_, err := daggerv1connect.NewGraphServiceClient(ts.Client(), ts.URL).
			GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(graph.seen).NotTo(Receive())
	})

	It("leaves the health check outside the principal gate", func() {
		client := serve(interceptor.StaticLookup{}, server.Services{}, interceptor.UnixResolver{})
		res, err := client.Get(baseURL + server.HealthPath)
		Expect(err).NotTo(HaveOccurred())
		defer res.Body.Close() //nolint:errcheck // response body of a finished read
		// Unauthenticated would be 401; the check answers on its own terms.
		Expect(res.StatusCode).NotTo(Equal(http.StatusUnauthorized))
	})
})
