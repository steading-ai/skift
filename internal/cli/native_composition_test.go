package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/mcphttp"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
)

type nativeCompositionGraph struct {
	skiftv1connect.UnimplementedGraphServiceHandler
	principal chan interceptor.Principal
}

func (g nativeCompositionGraph) ListProjects(ctx context.Context, _ *connect.Request[skiftv1.ListProjectsRequest]) (*connect.Response[skiftv1.ListProjectsResponse], error) {
	p, _ := interceptor.PrincipalFromContext(ctx)
	g.principal <- p
	return connect.NewResponse(&skiftv1.ListProjectsResponse{}), nil
}

type nativeCompositionWork struct {
	skiftv1connect.UnimplementedWorkServiceHandler
	started chan struct{}
	stopped chan struct{}
}

func (w nativeCompositionWork) ClaimNext(ctx context.Context, _ *connect.Request[skiftv1.ClaimNextRequest]) (*connect.Response[skiftv1.ClaimNextResponse], error) {
	close(w.started)
	<-ctx.Done()
	close(w.stopped)
	return nil, ctx.Err()
}
func nativeRequest(method string, args map[string]any) *http.Request {
	params := map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcphttp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
	for k, v := range args {
		params[k] = v
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	Expect(err).NotTo(HaveOccurred())
	r := httptest.NewRequest("POST", "http://skift.test/mcp", strings.NewReader(string(body)))
	r.Header.Set("MCP-Protocol-Version", mcphttp.ProtocolVersion)
	r.Header.Set("Mcp-Method", method)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if name, ok := params["name"].(string); ok {
		r.Header.Set("Mcp-Name", name)
	}
	return r
}

var _ = Describe("Native MCP service composition", Label("unit"), func() {
	var h http.Handler
	var graph nativeCompositionGraph
	var work nativeCompositionWork
	var checks *atomic.Int32
	BeforeEach(func() {
		graph = nativeCompositionGraph{principal: make(chan interceptor.Principal, 1)}
		work = nativeCompositionWork{started: make(chan struct{}), stopped: make(chan struct{})}
		checks = &atomic.Int32{}
		resolver := interceptor.TailscaleResolver{WhoIs: interceptor.WhoIsFunc(func(_ context.Context, peer string) (string, error) { checks.Add(1); return peer, nil })}
		lookup := interceptor.StaticLookup{{Scheme: interceptor.SchemeTailscale, Value: "original-peer"}: {ID: "alice", OrgID: "org", Kind: interceptor.KindAgent}}
		srv := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver)}}, server.Services{Graph: graph, Work: work}, nil)
		h = newNativeMCPHandler(srv, interceptor.NewAuthenticator(lookup, resolver))
	})
	It("uses the real factory for authenticated discover/list/call and server-side reauthentication", func() {
		for _, method := range []string{"server/discover", "tools/list", "tools/call"} {
			var params map[string]any
			if method == "tools/call" {
				params = map[string]any{"name": "project_list", "arguments": map[string]any{}}
			}
			for _, peer := range []string{"unknown", "original-peer"} {
				r := nativeRequest(method, params)
				r = r.WithContext(interceptor.WithTailnetPeer(r.Context(), peer))
				r.Header.Set("Forwarded", "for=original-peer")
				r.Header.Set("X-Skift-Principal", "alice")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if peer == "unknown" {
					Expect(w.Code).To(Equal(401))
				} else {
					Expect(w.Code).To(Equal(200), w.Body.String())
				}
			}
		}
		Expect(checks.Load()).To(Equal(int32(7))) // six outer HTTP checks plus generated Connect.
		Expect(graph.principal).To(Receive(Equal(interceptor.Principal{ID: "alice", OrgID: "org", Kind: interceptor.KindAgent})))
		Expect(graph.principal).NotTo(Receive())
	})
	It("propagates an actual HTTP disconnect through the SDK and generated waiting ClaimNext", func() {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(interceptor.WithTailnetPeer(r.Context(), "original-peer")))
		}))
		defer ts.Close()
		r := nativeRequest("tools/call", map[string]any{"name": "claim", "arguments": map[string]any{"projects": []string{"org/demo"}, "wait": "5m"}})
		body, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		outbound, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/mcp", strings.NewReader(string(body)))
		Expect(err).NotTo(HaveOccurred())
		outbound.Header = r.Header
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			res, err := ts.Client().Do(outbound)
			if err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				Expect(res.Body.Close()).To(Succeed())
			}
		}()
		Eventually(work.started).Should(BeClosed())
		cancel()
		Eventually(work.stopped).Should(BeClosed())
		Eventually(done).Should(BeClosed())
	})
})
