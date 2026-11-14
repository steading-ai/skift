package mcphttp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/mcphttp"
	"github.com/steading-ai/skift/internal/mcptools"
)

type graphStub struct {
	skiftv1connect.GraphServiceClient
	calls atomic.Int32
}

func (g *graphStub) ListProjects(context.Context, *connect.Request[skiftv1.ListProjectsRequest]) (*connect.Response[skiftv1.ListProjectsResponse], error) {
	g.calls.Add(1)
	return connect.NewResponse(&skiftv1.ListProjectsResponse{}), nil
}

func metadata() map[string]any {
	return map[string]any{"io.modelcontextprotocol/protocolVersion": mcphttp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
}
func request(method string, params map[string]any) *http.Request {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = metadata()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	Expect(err).NotTo(HaveOccurred())
	r := httptest.NewRequest(http.MethodPost, "http://skift.test/mcp", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", mcphttp.ProtocolVersion)
	r.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		r.Header.Set("Mcp-Name", name)
	}
	return r
}
func callRequest() *http.Request {
	return request("tools/call", map[string]any{"name": "project_list", "arguments": map[string]any{}})
}
func responseJSON(w *httptest.ResponseRecorder) map[string]any {
	body := w.Body.String()
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		var data []string
		for line := range strings.SplitSeq(body, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
		Expect(data).To(HaveLen(1), body)
		body = data[0]
	}
	var result map[string]any
	Expect(json.Unmarshal([]byte(body), &result)).To(Succeed(), body)
	return result
}

var _ = Describe("Native MCP SDK framing", Label("unit"), func() {
	var h http.Handler
	var graph *graphStub
	run := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	BeforeEach(func() { graph = &graphStub{}; h = mcphttp.New(mcptools.Execution{Graph: graph}) })
	It("discovers, lists and calls directly without initialization or sessions", func() {
		w := run(request("server/discover", nil))
		Expect(w.Code).To(Equal(200), w.Body.String())
		result := responseJSON(w)["result"].(map[string]any)
		Expect(result["supportedVersions"]).To(Equal([]any{mcphttp.ProtocolVersion}))
		Expect(result["capabilities"]).To(Equal(map[string]any{"tools": map[string]any{}}))
		Expect(result["instructions"]).To(ContainSubstring("Heartbeat explicitly"))
		for _, r := range []*http.Request{request("tools/list", nil), callRequest()} {
			r.Header.Set("Mcp-Session-Id", "not-a-session")
			r.Header.Set("Last-Event-ID", "ignored")
			w = run(r)
			Expect(w.Code).To(Equal(200), w.Body.String())
			Expect(w.Header().Get("Content-Type")).To(Equal("text/event-stream"))
			Expect(w.Header().Get("X-Accel-Buffering")).To(Equal("no"))
			Expect(w.Header().Get("Cache-Control")).To(Equal("no-cache, no-transform"))
			Expect(w.Header().Get("Mcp-Session-Id")).To(BeEmpty())
			Expect(w.Body.String()).NotTo(ContainSubstring("id:"))
			result := responseJSON(w)["result"].(map[string]any)
			Expect(result["_meta"]).To(HaveKey("io.modelcontextprotocol/serverInfo"))
			if r.Header.Get("Mcp-Method") == "tools/call" {
				Expect(result).To(HaveKey("structuredContent"))
			} else {
				Expect(result["tools"]).To(HaveLen(17))
			}
			Expect(r.Header.Get("Mcp-Session-Id")).To(Equal("not-a-session"))
			Expect(r.Header.Get("Last-Event-ID")).To(Equal("ignored"))
		}
		Expect(graph.calls.Load()).To(Equal(int32(1)))
	})
	It("serves the official 2026 SDK client without initialize, sessions or GET", func(ctx SpecContext) {
		methods := make(chan string, 8)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			methods <- r.Method + " " + r.Header.Get("Mcp-Method")
			h.ServeHTTP(w, r)
		}))
		defer ts.Close()
		client := mcp.NewClient(&mcp.Implementation{Name: "native-spec", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: ts.Client()}, &mcp.ClientSessionOptions{ProtocolVersion: mcphttp.ProtocolVersion})
		Expect(err).NotTo(HaveOccurred())
		tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.Tools).To(HaveLen(17))
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "project_list", Arguments: map[string]any{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsError).To(BeFalse())
		Expect(result.StructuredContent).NotTo(BeNil())
		Expect(session.Close()).To(Succeed())
		for _, method := range []string{"POST server/discover", "POST tools/list", "POST tools/call"} {
			Expect(methods).To(Receive(Equal(method)))
		}
		Expect(methods).NotTo(Receive())
	})
	DescribeTable("rejects older official SDK clients without executing tools", func(ctx SpecContext, version string) {
		statuses := make(chan int, 4)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			recorded := httptest.NewRecorder()
			h.ServeHTTP(recorded, r)
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			statuses <- recorded.Code
		}))
		defer ts.Close()
		client := mcp.NewClient(&mcp.Implementation{Name: "legacy-spec", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: ts.Client()}, &mcp.ClientSessionOptions{ProtocolVersion: version})
		if session != nil {
			_ = session.Close()
		}
		Expect(err).To(HaveOccurred())
		Expect(statuses).To(Receive(Equal(http.StatusBadRequest)))
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("2024-11-05", "2024-11-05"), Entry("2025-03-26", "2025-03-26"), Entry("2025-06-18", "2025-06-18"), Entry("2025-11-25", "2025-11-25"))
	DescribeTable("rejects each older HTTP revision before tools execute", func(version string) {
		r := callRequest()
		r.Header.Set("MCP-Protocol-Version", version)
		Expect(run(r).Code).To(Equal(400))
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("2024-11-05", "2024-11-05"), Entry("2025-03-26", "2025-03-26"), Entry("2025-06-18", "2025-06-18"), Entry("2025-11-25", "2025-11-25"), Entry("headerless legacy", ""))
	DescribeTable("validates mirrored headers", func(header, value string) {
		r := callRequest()
		r.Header.Set(header, value)
		w := run(r)
		Expect(w.Code).To(Equal(400), w.Body.String())
		if header != "MCP-Protocol-Version" || value != "" {
			Expect(responseJSON(w)["error"]).To(HaveKeyWithValue("code", float64(-32020)))
		}
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("missing method", "Mcp-Method", ""), Entry("different method", "Mcp-Method", "tools/list"), Entry("missing name", "Mcp-Name", ""), Entry("different name", "Mcp-Name", "ready"))
	It("returns SDK version data for an unsupported modern revision", func() {
		r := callRequest()
		r.Header.Set("MCP-Protocol-Version", "2027-01-01")
		raw, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())
		r.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(raw), mcphttp.ProtocolVersion, "2027-01-01")))
		w := run(r)
		Expect(w.Code).To(Equal(400), w.Body.String())
		e := responseJSON(w)["error"].(map[string]any)
		Expect(e["code"]).To(Equal(float64(-32022)))
		Expect(e).To(HaveKey("data"))
		Expect(graph.calls.Load()).To(BeZero())
	})
	DescribeTable("requires valid per-request metadata", func(meta any) {
		r := callRequest()
		raw, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())
		var body map[string]any
		Expect(json.Unmarshal(raw, &body)).To(Succeed())
		params := body["params"].(map[string]any)
		params["_meta"] = meta
		raw, err = json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		w := run(r)
		Expect(w.Code).To(Equal(400), w.Body.String())
		Expect(responseJSON(w)).To(HaveKey("error"))
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("absent", nil), Entry("not an object", "bad"), Entry("missing version", map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{}}), Entry("missing capabilities", map[string]any{"io.modelcontextprotocol/protocolVersion": mcphttp.ProtocolVersion}), Entry("capabilities not object", map[string]any{"io.modelcontextprotocol/protocolVersion": mcphttp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": "bad"}), Entry("version wrong type", map[string]any{"io.modelcontextprotocol/protocolVersion": 42, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}), Entry("version mismatch", map[string]any{"io.modelcontextprotocol/protocolVersion": "2027-01-01", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}))
	It("normalizes a known encoded name once without mutating caller headers", func() {
		r := callRequest()
		name := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("project_list")) + "?="
		r.Header.Set("Mcp-Name", name)
		w := run(r)
		Expect(w.Code).To(Equal(200), w.Body.String())
		Expect(graph.calls.Load()).To(Equal(int32(1)))
		Expect(r.Header.Get("Mcp-Name")).To(Equal(name))
	})
	DescribeTable("leaves encoded mismatches and unsafe names for SDK rejection", func(name string) {
		r := callRequest()
		r.Header.Set("Mcp-Name", name)
		w := run(r)
		Expect(w.Code).To(Equal(400), w.Body.String())
		Expect(responseJSON(w)["error"]).To(HaveKeyWithValue("code", float64(-32020)))
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("different known name", "=?base64?cmVhZHk=?="), Entry("malformed", "=?base64?!!?="), Entry("unsafe", "=?base64?cHJvamVjdF9saXN0Cg==?="), Entry("unknown", "=?base64?dW5rbm93bg==?="), Entry("double encoding", "=?base64?"+base64.StdEncoding.EncodeToString([]byte("=?base64?cHJvamVjdF9saXN0?="))+"?="))
	DescribeTable("allows only POST", func(method string) {
		r := callRequest()
		r.Method = method
		w := run(r)
		Expect(w.Code).To(Equal(405))
		Expect(w.Header().Get("Allow")).To(Equal("POST"))
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("GET", "GET"), Entry("DELETE", "DELETE"), Entry("HEAD", "HEAD"), Entry("PUT", "PUT"), Entry("PATCH", "PATCH"), Entry("OPTIONS", "OPTIONS"))
	DescribeTable("rejects incomplete Accept", func(accept string) {
		r := callRequest()
		r.Header.Set("Accept", accept)
		Expect(run(r).Code).To(Equal(400))
		Expect(graph.calls.Load()).To(BeZero())
	}, Entry("json only", "application/json"), Entry("SSE only", "text/event-stream"), Entry("absent", ""))
	It("rejects content types, malformed and oversize bodies without a tool call", func() {
		r := callRequest()
		r.Header.Set("Content-Type", "text/plain")
		Expect(run(r).Code).To(Equal(415))
		r = callRequest()
		r.Body = io.NopCloser(strings.NewReader("{"))
		Expect(run(r).Code).To(Equal(400))
		r = callRequest()
		r.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", 4*1024*1024+1)))
		Expect(run(r).Code).To(Equal(413))
		Expect(graph.calls.Load()).To(BeZero())
	})
	It("returns method-not-found for unknown RPCs and unadvertised subscriptions without hanging", func() {
		for _, method := range []string{"not/a/method", "subscriptions/listen", "resources/list", "prompts/list"} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r := request(method, nil).WithContext(ctx)
			w := run(r)
			Expect(w.Code).To(Equal(404), w.Body.String())
			Expect(responseJSON(w)["error"]).To(HaveKeyWithValue("code", float64(-32601)))
		}
	})
	It("accepts notifications with an empty 202 response", func() {
		r := request("notifications/initialized", nil)
		raw, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())
		r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(raw), `"id":1,`, "", 1)))
		w := run(r)
		Expect(w.Code).To(Equal(202), w.Body.String())
		Expect(w.Body.Len()).To(BeZero())
	})
	It("keeps tool errors distinct from protocol errors", func() {
		r := request("tools/call", map[string]any{"name": "project_list", "arguments": map[string]any{"unknown": true}})
		w := run(r)
		Expect(w.Code).To(Equal(200), w.Body.String())
		Expect(responseJSON(w)["result"]).To(HaveKeyWithValue("isError", true))
		Expect(graph.calls.Load()).To(BeZero())
	})
})
