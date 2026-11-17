package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/mcphttp"
	"github.com/steading-ai/skift/internal/mcptools"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
	"github.com/steading-ai/skift/internal/testsupport"
)

type mcpLeaseTransport struct{ isolatedTransport }

func (*mcpLeaseTransport) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	return interceptor.TailnetConnContext(ctx, conn)
}

type httpToolResult struct {
	IsError           bool                       `json:"isError"`
	StructuredContent map[string]json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func mcpToolRequest(ctx context.Context, url, name string, args map[string]any) *http.Request {
	GinkgoHelper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": newID(), "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcphttp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/mcp", bytes.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcphttp.ProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", name)
	return req
}

func callHTTPTool(ctx context.Context, ts *httptest.Server, name string, args map[string]any) httpToolResult {
	GinkgoHelper()
	res, err := ts.Client().Do(mcpToolRequest(ctx, ts.URL, name, args))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(res.StatusCode).To(Equal(http.StatusOK), string(raw))
	Expect(res.Header.Get("Content-Type")).To(HavePrefix("text/event-stream"))
	Expect(res.Header.Get("Mcp-Session-Id")).To(BeEmpty())
	var result httpToolResult
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		Expect(json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &envelope)).To(Succeed())
		Expect(envelope.Error).To(BeEmpty(), string(raw))
		if len(envelope.Result) > 0 {
			Expect(json.Unmarshal(envelope.Result, &result)).To(Succeed())
			found = true
		}
	}
	Expect(scanner.Err()).NotTo(HaveOccurred())
	Expect(found).To(BeTrue(), string(raw))
	return result
}

func httpLeaseID(result httpToolResult) string {
	GinkgoHelper()
	Expect(result.IsError).To(BeFalse(), "%+v", result.Content)
	var lease struct {
		ID string `json:"id"`
	}
	Expect(json.Unmarshal(result.StructuredContent["lease"], &lease)).To(Succeed())
	Expect(lease.ID).NotTo(BeEmpty())
	return lease.ID
}

var _ = Describe("HTTP MCP real-domain lease lifetime", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ts *httptest.Server
	var active atomic.Int32
	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		f.project(ctx, "Platform")
		svc := domain.NewService(f.pool, pol)
		lookup := interceptor.StaticLookup{{Scheme: interceptor.SchemeTailscale, Value: "node"}: f.user}
		resolver := interceptor.TailscaleResolver{WhoIs: interceptor.WhoIsFunc(func(context.Context, string) (string, error) { return "node", nil })}
		observe := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if req.Spec().Procedure == skiftv1connect.WorkServiceClaimNextProcedure {
					active.Add(1)
					defer active.Add(-1)
				}
				return next(ctx, req)
			}
		})
		srv := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver), observe}}, server.Services{Graph: server.NewGraphService(svc), Work: server.NewWorkService(svc)}, f.pool)
		execution := mcptools.Execution{Draining: server.Draining, Graph: skiftv1connect.NewGraphServiceClient(srv.InProcessClient(), "http://skift.internal"), Work: skiftv1connect.NewWorkServiceClient(srv.InProcessClient(), "http://skift.internal")}
		ts = httptest.NewUnstartedServer(interceptor.NewAuthenticator(lookup, resolver).Middleware(mcphttp.New(execution)))
		ts.Config.ConnContext = interceptor.TailnetConnContext
		ts.Start()
		DeferCleanup(ts.Close)
	})
	It("retains a committed claim after SSE closes and uses separate explicit heartbeat and completion calls", func(ctx SpecContext) {
		f.mustApply(ctx, createOp("task", "Work"))
		claimed := callHTTPTool(ctx, ts, "claim", map[string]any{"projects": []string{"platform"}, "ttl": "5s"})
		token := httpLeaseID(claimed)
		got := f.getNode(ctx, "platform", "#1")
		Expect(got.GetLease().GetId()).To(Equal(token))
		Expect(got.GetLease().GetHolder()).To(Equal(f.user.ID))
		expiry := got.GetLease().GetExpiresAt().AsTime()
		// Reading the entire SSE body above observed EOF; no connection/session owns this lease.
		Consistently(func() time.Time { return f.getNode(ctx, "platform", "#1").GetLease().GetExpiresAt().AsTime() }, 200*time.Millisecond, 50*time.Millisecond).Should(Equal(expiry))
		renewed := callHTTPTool(ctx, ts, "heartbeat", map[string]any{"lease_id": token, "ttl": "10s"})
		Expect(httpLeaseID(renewed)).To(Equal(token))
		Expect(f.getNode(ctx, "platform", "#1").GetLease().GetExpiresAt().AsTime()).To(BeTemporally(">", expiry))
		completed := callHTTPTool(ctx, ts, "complete", map[string]any{"ref": "platform#1", "lease_id": token, "result": "done"})
		Expect(completed.IsError).To(BeFalse(), "%+v", completed.Content)
		got = f.getNode(ctx, "platform", "#1")
		Expect(got.GetNode().GetStatus()).To(Equal(skiftv1.Status_STATUS_DONE))
		Expect(got.GetLease()).To(BeNil())
	})
	It("releases only by explicit call and fences an expired claim after re-claim", func(ctx SpecContext) {
		f.mustApply(ctx, createOp("task", "Work"))
		token := httpLeaseID(callHTTPTool(ctx, ts, "claim", map[string]any{"projects": []string{"platform"}, "ttl": "100ms"}))
		Eventually(func() bool {
			return time.Now().After(f.getNode(ctx, "platform", "#1").GetLease().GetExpiresAt().AsTime())
		}).Should(BeTrue())
		stale := callHTTPTool(ctx, ts, "heartbeat", map[string]any{"lease_id": token})
		Expect(stale.IsError).To(BeTrue())
		Expect(stale.Content[0].Text).To(ContainSubstring("lease is not live"))
		next := httpLeaseID(callHTTPTool(ctx, ts, "claim", map[string]any{"projects": []string{"platform"}}))
		Expect(next).NotTo(Equal(token))
		stale = callHTTPTool(ctx, ts, "complete", map[string]any{"ref": "platform#1", "lease_id": token, "result": "late"})
		Expect(stale.IsError).To(BeTrue())
		Expect(f.getNode(ctx, "platform", "#1").GetLease().GetId()).To(Equal(next))
		released := callHTTPTool(ctx, ts, "release", map[string]any{"lease_id": next, "reason": "preempted"})
		Expect(released.IsError).To(BeFalse(), "%+v", released.Content)
		got := f.getNode(ctx, "platform", "#1")
		Expect(got.GetLease()).To(BeNil())
		Expect(got.GetNode().GetReady()).To(BeTrue())
	})
	It("wakes a real waiting HTTP claim on common service drain and leaves committed leases live", func(ctx SpecContext) {
		f.mustApply(ctx, createOp("task", "Work"))
		token := httpLeaseID(callHTTPTool(ctx, ts, "claim", map[string]any{"projects": []string{"platform"}}))
		svc := domain.NewService(f.pool, pol)
		lookup := interceptor.StaticLookup{{Scheme: interceptor.SchemeTailscale, Value: "node"}: f.user}
		resolver := interceptor.TailscaleResolver{WhoIs: interceptor.WhoIsFunc(func(context.Context, string) (string, error) { return "node", nil })}
		entered := make(chan struct{}, 1)
		observe := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if req.Spec().Procedure == skiftv1connect.WorkServiceClaimNextProcedure {
					entered <- struct{}{}
				}
				return next(ctx, req)
			}
		})
		var handler http.Handler
		transport := &mcpLeaseTransport{isolatedTransport: isolatedTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) })}}
		run := start(server.Config{Transports: []server.Transport{transport}}, server.Services{}, f.pool)
		// Inject the real generated services into a dispatcher whose request contexts
		// originate on the running listener and therefore carry its common drain signal.
		dispatcher := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver), observe}}, server.Services{Graph: server.NewGraphService(svc), Work: server.NewWorkService(svc)}, f.pool)
		handler = interceptor.NewAuthenticator(lookup, resolver).Middleware(mcphttp.New(mcptools.Execution{
			Draining: server.Draining,
			Graph:    skiftv1connect.NewGraphServiceClient(dispatcher.InProcessClient(), "http://skift.internal"), Work: skiftv1connect.NewWorkServiceClient(dispatcher.InProcessClient(), "http://skift.internal"),
		}))
		url := "http://" + transport.listener.Addr().String()
		req := mcpToolRequest(ctx, url, "claim", map[string]any{"projects": []string{"platform"}, "wait": "1m"})
		done := make(chan []byte, 1)
		go func() {
			defer GinkgoRecover()
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = res.Body.Close() }()
			raw, err := io.ReadAll(res.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.StatusCode).To(Equal(http.StatusOK))
			done <- raw
		}()
		Eventually(entered).Should(Receive())
		Expect(run.stop()).To(Succeed())
		var raw []byte
		Eventually(done).Should(Receive(&raw))
		Expect(string(raw)).To(ContainSubstring("structuredContent"))
		Expect(string(raw)).NotTo(ContainSubstring(`"isError":true`))
		Expect(f.getNode(ctx, "platform", "#1").GetLease().GetId()).To(Equal(token))
	})
	It("propagates HTTP disconnect cancellation to a real waiting ClaimNext", func(ctx SpecContext) {
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			res, err := ts.Client().Do(mcpToolRequest(callCtx, ts.URL, "claim", map[string]any{"projects": []string{"platform"}, "wait": "1m"}))
			if res != nil {
				_ = res.Body.Close()
			}
			done <- err
		}()
		Eventually(active.Load).Should(Equal(int32(1)))
		cancel()
		Eventually(done).Should(Receive(HaveOccurred()))
		Eventually(active.Load).Should(BeZero())
		f.mustApply(ctx, createOp("after", "Still available"))
		token := httpLeaseID(callHTTPTool(ctx, ts, "claim", map[string]any{"projects": []string{"platform"}}))
		Expect(token).NotTo(BeEmpty())
	})
})
