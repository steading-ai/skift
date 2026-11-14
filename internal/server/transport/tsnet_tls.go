package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// DefaultMCPHTTPPort is the opt-in native MCP TLS port.
const DefaultMCPHTTPPort uint16 = 443

type tlsListenerNode interface {
	ListenTLS(network, addr string) (net.Listener, error)
}

// TailnetTLS shares Tailnet's enrolled node, peer identity and lifetime. Only
// its own listener is closed by the server; Close never closes the shared node.
type TailnetTLS struct {
	parent  *Tailnet
	node    tlsListenerNode
	port    uint16
	handler http.Handler
}

// NewTailnetTLS prepares an isolated MCP listener. The caller must inject the
// authenticated native MCP handler; no protocol or unauthenticated fallback is
// provided. Register parent before this transport in the server configuration.
func NewTailnetTLS(parent *Tailnet, port uint16, handler http.Handler) (*TailnetTLS, error) {
	if parent == nil {
		return nil, errors.New("transport: MCP HTTPS requires a tailnet node")
	}
	if handler == nil {
		return nil, errors.New("transport: MCP HTTPS requires an authenticated MCP handler")
	}
	if port == 0 {
		port = DefaultMCPHTTPPort
	}
	if port == parent.cfg.Port {
		return nil, errors.New("transport: MCP HTTPS port collides with Connect port")
	}
	return &TailnetTLS{parent: parent, node: parent.ts, port: port, handler: handler}, nil
}

func (t *TailnetTLS) Name() string { return "tailnet-mcp-https" }

// Listen uses tsnet's tailnet-only TLS API. MagicDNS and HTTPS must be enabled
// in the tailnet admin console. Certificate retrieval is lazy at handshake;
// binding successfully is not evidence that a certificate was issued.
func (t *TailnetTLS) Listen() (net.Listener, error) {
	if t.parent.certificateDNS == "" {
		return nil, errors.New("transport: MCP HTTPS requires tailnet MagicDNS and HTTPS certificate DNS configuration")
	}
	l, err := t.node.ListenTLS("tcp", fmt.Sprintf(":%d", t.port))
	if err != nil {
		return nil, fmt.Errorf("transport: MCP HTTPS (enable MagicDNS and HTTPS in the tailnet): %w", err)
	}
	return l, nil
}

func (t *TailnetTLS) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	return t.parent.ConnContext(ctx, conn)
}

func (t *TailnetTLS) Close() error { return nil }

// HTTPHandler isolates the native endpoint from the ordinary Connect mux.
// Host and Origin are checked explicitly: neither proxy headers nor the SDK's
// loopback Host policy establish trust on a tailnet listener.
func (t *TailnetTLS) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		if !t.validHost(r.Host) || !t.validOrigin(r.Header.Values("Origin")) {
			http.Error(w, "untrusted MCP Host or Origin", http.StatusForbidden)
			return
		}
		t.handler.ServeHTTP(w, r)
	})
}

func (t *TailnetTLS) validHost(host string) bool {
	dns := t.parent.certificateDNS
	if dns == "" {
		return false
	}
	authority := net.JoinHostPort(dns, strconv.Itoa(int(t.port)))
	return strings.EqualFold(host, authority) || (t.port == DefaultMCPHTTPPort && strings.EqualFold(host, dns))
}

func (t *TailnetTLS) validOrigin(origins []string) bool {
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	authority, ok := strings.CutPrefix(origins[0], "https://")
	return ok && t.validHost(authority)
}
