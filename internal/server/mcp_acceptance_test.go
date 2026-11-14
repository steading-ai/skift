package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// lostMCPResponse consumes one committed response without delivering it to the
// client. A retry must use the domain request_id, not the JSON-RPC id.
type lostMCPResponse struct {
	base http.RoundTripper
	lose atomic.Bool
}

func (t *lostMCPResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(r)
	if err == nil && t.lose.Swap(false) {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return nil, errors.New("injected lost response")
	}
	return res, err
}

func sdkTool(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	GinkgoHelper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	Expect(err).NotTo(HaveOccurred())
	Expect(result.IsError).To(BeFalse(), "%+v", result.Content)
	return result
}
func sdkContent(result *mcp.CallToolResult) map[string]json.RawMessage {
	GinkgoHelper()
	raw, err := json.Marshal(result.StructuredContent)
	Expect(err).NotTo(HaveOccurred())
	var content map[string]json.RawMessage
	Expect(json.Unmarshal(raw, &content)).To(Succeed())
	return content
}
func sdkLease(result *mcp.CallToolResult) string {
	GinkgoHelper()
	var lease struct {
		ID string `json:"id"`
	}
	Expect(json.Unmarshal(sdkContent(result)["lease"], &lease)).To(Succeed())
	Expect(lease.ID).NotTo(BeEmpty())
	return lease.ID
}

var _ = Describe("Native MCP SDK acceptance", Label(testsupport.DBLabel), func() {
	var f *fixture
	var handler http.Handler
	var open func(context.Context, string) (*mcp.ClientSession, *lostMCPResponse)
	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		other := interceptor.Principal{ID: newID(), OrgID: newID(), Kind: interceptor.KindUser}
		_, err := f.pool.Exec(ctx, `INSERT INTO organizations(id,name) VALUES($1,'other')`, other.OrgID)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.pool.Exec(ctx, `INSERT INTO principals(id,org_id,kind) VALUES($1,$2,'user')`, other.ID, other.OrgID)
		Expect(err).NotTo(HaveOccurred())
		principals := map[string]interceptor.Principal{"user": f.user, "agent": f.agent, "other": other}
		for node, p := range principals {
			_, err = f.pool.Exec(ctx, `INSERT INTO principal_identities(principal_id,scheme,value) VALUES($1,'tailscale',$2)`, p.ID, node)
			Expect(err).NotTo(HaveOccurred())
		}
		svc := domain.NewService(f.pool, pol)
		lookup := server.NewIdentityLookup(svc)
		resolver := interceptor.TailscaleResolver{WhoIs: interceptor.WhoIsFunc(func(_ context.Context, peer string) (string, error) { return peer, nil })}
		srv := server.New(server.Config{Interceptors: []connect.Interceptor{interceptor.NewPrincipal(lookup, resolver)}}, server.Services{Graph: server.NewGraphService(svc), Work: server.NewWorkService(svc)}, f.pool)
		handler = interceptor.NewAuthenticator(lookup, resolver).Middleware(mcphttp.New(mcptools.Execution{Graph: skiftv1connect.NewGraphServiceClient(srv.InProcessClient(), "http://skift.internal"), Work: skiftv1connect.NewWorkServiceClient(srv.InProcessClient(), "http://skift.internal")}))
		open = func(ctx context.Context, peer string) (*mcp.ClientSession, *lostMCPResponse) {
			GinkgoHelper()
			ts := httptest.NewUnstartedServer(handler)
			// Each listener models a separately accepted node connection, not a caller header.
			ts.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context { return interceptor.WithTailnetPeer(ctx, peer) }
			ts.Start()
			DeferCleanup(ts.Close)
			lost := &lostMCPResponse{base: ts.Client().Transport}
			client := mcp.NewClient(&mcp.Implementation{Name: "acceptance", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: lost}}, &mcp.ClientSessionOptions{ProtocolVersion: mcphttp.ProtocolVersion})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(session.Close()).To(Succeed()) })
			return session, lost
		}
	})
	It("completes a dependency chain across connections and replays a lost write response once", func(ctx SpecContext) {
		first, lost := open(ctx, "user")
		tools, err := first.ListTools(ctx, &mcp.ListToolsParams{})
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.Tools).To(HaveLen(17))
		sdkTool(ctx, first, "project_create", map[string]any{"title": "Platform"})
		requestID := newID()
		args := map[string]any{"project": "platform", "request_id": requestID, "view": "full", "ops": []any{
			map[string]any{"create_node": map[string]any{"alias": "before", "title": "Predecessor"}},
			map[string]any{"create_node": map[string]any{"alias": "after", "title": "Successor", "blocked_by": []string{"before"}}},
		}}
		lost.lose.Store(true)
		_, err = first.CallTool(ctx, &mcp.CallToolParams{Name: "apply_patch", Arguments: args})
		Expect(err).To(HaveOccurred())
		second, _ := open(ctx, "user")
		replay := sdkTool(ctx, second, "apply_patch", args)
		var events []struct {
			Principal      string `json:"principal"`
			IdentityScheme string `json:"identity_scheme"`
		}
		Expect(json.Unmarshal(sdkContent(replay)["events"], &events)).To(Succeed())
		Expect(events).To(HaveLen(3))
		for _, event := range events {
			Expect(event.Principal).To(Equal(f.user.ID))
			Expect(event.IdentityScheme).To(Equal("tailscale"))
		}
		nodes, err := f.graph().ListNodes(ctx, connect.NewRequest(&skiftv1.ListNodesRequest{Filter: &skiftv1.NodeFilter{Projects: []string{"platform"}}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes.Msg.GetNodes()).To(HaveLen(2))
		claimed := sdkTool(ctx, second, "claim", map[string]any{"projects": []string{"platform"}, "request_id": newID()})
		token := sdkLease(claimed)
		Expect(string(sdkContent(claimed)["node"])).To(ContainSubstring("Predecessor"))
		sdkTool(ctx, first, "heartbeat", map[string]any{"lease_id": token, "request_id": newID()})
		sdkTool(ctx, first, "complete", map[string]any{"ref": "platform#1", "lease_id": token, "result": "predecessor evidence", "request_id": newID()})
		Expect(f.getNode(ctx, "platform", "#2").GetNode().GetReady()).To(BeTrue())
		read := sdkTool(ctx, second, "show_node", map[string]any{"ref": "platform#2"})
		Expect(string(sdkContent(read)["node"])).To(ContainSubstring("Successor"))
		next := sdkTool(ctx, second, "claim", map[string]any{"projects": []string{"platform"}})
		Expect(string(sdkContent(next)["node"])).To(ContainSubstring("Successor"))
		inputs := sdkTool(ctx, first, "inputs", map[string]any{"ref": "platform#2"})
		Expect(string(sdkContent(inputs)["inputs"])).To(ContainSubstring("predecessor evidence"))
		sdkTool(ctx, second, "complete", map[string]any{"ref": "platform#2", "lease_id": sdkLease(next), "result": "done"})
		Expect(f.getNode(ctx, "platform", "#2").GetNode().GetStatus()).To(Equal(skiftv1.Status_STATUS_DONE))
	})
	It("denies cross-organization reads and writes on separate SDK connections without recording events", func(ctx SpecContext) {
		owner, _ := open(ctx, "user")
		stranger, _ := open(ctx, "other")
		sdkTool(ctx, owner, "project_create", map[string]any{"title": "Platform"})
		sdkTool(ctx, owner, "apply_patch", map[string]any{"project": "platform", "ops": []any{map[string]any{"create_node": map[string]any{"title": "Private"}}}})
		var before, after int
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&before)).To(Succeed())
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"show_node", map[string]any{"ref": "platform#1"}},
			{"apply_patch", map[string]any{"project": "platform", "ops": []any{map[string]any{"create_node": map[string]any{"title": "Intruder"}}}}},
		} {
			result, err := stranger.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsError).To(BeTrue())
		}
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&after)).To(Succeed())
		Expect(after).To(Equal(before))
		Expect(f.getNode(ctx, "platform", "#1").GetNode().GetTitle()).To(Equal("Private"))
	})
	It("retains gate and resolved-patch policy denial through the SDK", func(ctx SpecContext) {
		user, _ := open(ctx, "user")
		agent, _ := open(ctx, "agent")
		sdkTool(ctx, user, "project_create", map[string]any{"title": "Platform"})
		sdkTool(ctx, user, "apply_patch", map[string]any{"project": "platform", "ops": []any{map[string]any{"create_node": map[string]any{"title": "Gate", "assignee_kind": "user"}}}})
		var before, after int
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&before)).To(Succeed())
		for _, ops := range [][]any{
			{map[string]any{"complete_node": map[string]any{"node": "#1", "result": map[string]any{"body": "unauthorized"}}}},
			{map[string]any{"create_node": map[string]any{"title": "Allowed root"}}, map[string]any{"create_node": map[string]any{"title": "Denied child", "parent": "#1"}}},
			{map[string]any{"hold_node": map[string]any{"node": "#1", "reason": "unauthorized"}}},
		} {
			result, err := agent.CallTool(ctx, &mcp.CallToolParams{Name: "apply_patch", Arguments: map[string]any{"project": "platform", "ops": ops}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsError).To(BeTrue())
			Expect(result.Content[0].(*mcp.TextContent).Text).To(ContainSubstring("permission_denied"))
		}
		Expect(f.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&after)).To(Succeed())
		Expect(after).To(Equal(before))
		nodes, err := f.graph().ListNodes(ctx, connect.NewRequest(&skiftv1.ListNodesRequest{Filter: &skiftv1.NodeFilter{Projects: []string{"platform"}}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes.Msg.GetNodes()).To(HaveLen(1))
	})
})
