// Package mcphttp frames the shared tools with the official MCP SDK.
// Authentication and canonical Host/Origin enforcement belong to the listener.
package mcphttp

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/steading-ai/skift/internal/mcptools"
	"github.com/steading-ai/skift/internal/version"
)

const ProtocolVersion = "2026-07-28"

const instructions = `Use explicit project arguments or qualified node references; there is no current project or client session. Claims belong to the authenticated caller and remain live after the HTTP response or disconnect. Heartbeat explicitly before expiry, then complete or release using the returned lease and fencing token. There is no automatic renewal or shutdown release. Supply the same request_id when retrying a mutation after a lost response; a JSON-RPC ID is not an idempotency key. Cancellation may race a committed mutation: retry with that request_id or inspect the graph. Streams are request-scoped and cannot be resumed.`

// New builds a stateless handler. Execution deliberately has no caller defaults
// or lease hooks: HTTP response lifetime must never own a domain lease.
func New(execution mcptools.Execution) http.Handler {
	execution.Projects = nil
	execution.Harness, execution.AgentRef, execution.SessionRef = "", "", ""
	execution.Leases = nil
	tools := mcptools.New(execution)
	names := make(map[string]bool, len(tools))
	s := mcp.NewServer(&mcp.Implementation{Name: "skift", Version: version.String()}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{ProtocolVersion},
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		Instructions:              instructions,
	})
	for _, tool := range tools {
		names[tool.Name] = true
		a := tool.Annotations
		s.AddTool(&mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, OutputSchema: tool.OutputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: a.ReadOnlyHint, DestructiveHint: &a.DestructiveHint, IdempotentHint: a.IdempotentHint, OpenWorldHint: &a.OpenWorldHint}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result := tool.Invoke(ctx, req.Params.Arguments)
				content := make([]mcp.Content, 0, len(result.Content))
				for _, c := range result.Content {
					content = append(content, &mcp.TextContent{Text: c.Text})
				}
				return &mcp.CallToolResult{Content: content, StructuredContent: result.StructuredContent, IsError: result.IsError}, nil
			})
	}
	// Do not expose SDK built-ins outside the advertised tools-only feature set,
	// especially subscriptions/listen, which otherwise opens a long-lived stream.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "server/discover", "tools/list", "tools/call", "initialize", "ping", "notifications/initialized", "notifications/cancelled", "notifications/progress":
				return next(ctx, method, req)
			default:
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
			}
		}
	})
	sdkHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless: true, PropagateRequestCancellation: true, JSONResponse: false,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// v1.8.0 otherwise defaults headerless requests to legacy 2025-03-26.
		// No body parsing or legacy handshake is supported by this endpoint.
		if r.Header.Get("MCP-Protocol-Version") == "" {
			http.Error(w, "MCP-Protocol-Version is required", http.StatusBadRequest)
			return
		}
		// No session or replay authority exists on this stateless endpoint. The
		// SDK rejects Last-Event-ID on POST; discard it to honor our no-replay
		// contract rather than accidentally treating it as transport state.
		r = r.Clone(r.Context())
		r.Header.Del("Mcp-Session-Id")
		r.Header.Del("Last-Event-ID")
		r = normalizeName(r, names)
		w.Header().Set("X-Accel-Buffering", "no")
		sdkHandler.ServeHTTP(w, r)
	})
}

// v1.8.0 compares Mcp-Name raw, unlike Mcp-Param-* (ADR-011). Normalize
// exactly once and only for this registry's header-safe ASCII names. The SDK
// still checks the body; malformed/unknown/unsafe/double-encoded names stay raw.
func normalizeName(r *http.Request, names map[string]bool) *http.Request {
	value := r.Header.Get("Mcp-Name")
	encoded, ok := strings.CutPrefix(value, "=?base64?")
	if !ok {
		return r
	}
	encoded, ok = strings.CutSuffix(encoded, "?=")
	if !ok {
		return r
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded || !names[string(decoded)] {
		return r
	}
	for _, c := range decoded {
		if c < 33 || c > 126 {
			return r
		}
	}
	r = r.Clone(r.Context())
	r.Header.Set("Mcp-Name", string(decoded))
	return r
}
