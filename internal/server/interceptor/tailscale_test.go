package interceptor_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/server/interceptor"
)

// whoIsTable is a WhoIs answering with one node for every address, the way
// a single laptop reaching the service looks to tsnet.
type whoIsTable struct {
	node string
	err  error
	// asked records the addresses WhoIs was asked about.
	asked []string
}

func (w *whoIsTable) WhoIs(_ context.Context, remoteAddr string) (string, error) {
	w.asked = append(w.asked, remoteAddr)
	return w.node, w.err
}

// serveTailnet stands in for the tailnet transport: a loopback listener
// whose connections are marked as tsnet's are, with the principal
// interceptor in front of graph.
func serveTailnet(lookup interceptor.Lookup, graph skiftv1connect.GraphServiceHandler, whoIs interceptor.WhoIs) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle(skiftv1connect.NewGraphServiceHandler(graph, connect.WithInterceptors(
		interceptor.NewPrincipal(lookup, interceptor.UnixResolver{}, interceptor.TailscaleResolver{WhoIs: whoIs}),
	)))
	ts := httptest.NewUnstartedServer(mux)
	ts.Config.ConnContext = interceptor.TailnetConnContext
	ts.Start()
	DeferCleanup(ts.Close)
	return ts
}

var _ = Describe("Tailscale identity resolver", Label("unit"), func() {
	laptop := interceptor.Identity{Scheme: interceptor.SchemeTailscale, Value: "nLAPTOP01"}

	It("resolves a tailnet caller to a principal via its node", func(ctx SpecContext) {
		By("given a tailscale identity mapping the caller's node to a principal")
		whoIs := &whoIsTable{node: laptop.Value}
		graph := newRecordingGraph()
		ts := serveTailnet(interceptor.StaticLookup{laptop: alice}, graph, whoIs)

		By("when the node calls the service over the tailnet")
		_, err := skiftv1connect.NewGraphServiceClient(ts.Client(), ts.URL).
			GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "skeleton"}))
		Expect(err).NotTo(HaveOccurred())

		By("then the request is attributed to that principal through the tailscale scheme")
		var seen attribution
		Expect(graph.seen).To(Receive(&seen))
		Expect(seen.principal).To(Equal(alice))
		Expect(seen.identity).To(Equal(laptop))

		By("and WhoIs was asked about the connection's remote address")
		Expect(whoIs.asked).To(HaveLen(1))
		host, _, err := net.SplitHostPort(whoIs.asked[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(net.ParseIP(host).IsLoopback()).To(BeTrue())
	})

	It("A tailnet caller with no mapped identity is rejected", func(ctx SpecContext) {
		By("when an unmapped tailnet node calls the service")
		graph := newRecordingGraph()
		ts := serveTailnet(interceptor.StaticLookup{}, graph, &whoIsTable{node: "nSTRANGER"})
		_, err := skiftv1connect.NewGraphServiceClient(ts.Client(), ts.URL).
			GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "skeleton"}))

		By("then the call fails as unauthenticated")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(graph.seen).NotTo(Receive())
	})

	It("fails the call when the tailnet cannot say who is calling", func(ctx SpecContext) {
		graph := newRecordingGraph()
		ts := serveTailnet(interceptor.StaticLookup{laptop: alice}, graph, &whoIsTable{err: errors.New("no such peer")})
		_, err := skiftv1connect.NewGraphServiceClient(ts.Client(), ts.URL).
			GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "skeleton"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(err.Error()).To(ContainSubstring("no such peer"))
		Expect(graph.seen).NotTo(Receive())
	})

	It("declines a request that did not arrive over the tailnet without asking WhoIs", func(ctx SpecContext) {
		By("given the resolver behind a connection the tailnet transport did not mark")
		whoIs := &whoIsTable{node: laptop.Value}
		r := interceptor.TailscaleResolver{WhoIs: whoIs}

		By("when it resolves")
		_, err := r.Resolve(ctx, connect.Peer{Addr: "127.0.0.1:1"}, http.Header{})

		By("then it passes to the next resolver, and the tailnet was never consulted")
		Expect(err).To(MatchError(interceptor.ErrNoIdentity))
		Expect(whoIs.asked).To(BeEmpty())

		By("and a marked connection resolves to the node")
		id, err := r.Resolve(interceptor.WithTailnetPeer(ctx, "100.64.0.7:51234"), connect.Peer{}, http.Header{})
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal(laptop))
		Expect(whoIs.asked).To(Equal([]string{"100.64.0.7:51234"}))
	})
})
