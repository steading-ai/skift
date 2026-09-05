package interceptor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"connectrpc.com/connect"
)

// SchemeTailscale is the identity scheme of callers reaching the service
// over the tailnet.
const SchemeTailscale = "tailscale"

type tailnetPeerKey struct{}

// TailnetConnContext is the http.Server ConnContext of the tailnet
// transport. It marks every connection accepted from the tailnet with the
// peer's remote address, which is what [TailscaleResolver] asks WhoIs
// about. Connections from any other listener never pass through it, so the
// resolver declines them.
func TailnetConnContext(ctx context.Context, conn net.Conn) context.Context {
	return WithTailnetPeer(ctx, conn.RemoteAddr().String())
}

// WithTailnetPeer returns a context marking the request as having arrived
// over the tailnet from remoteAddr. The transport calls it through
// [TailnetConnContext]; specs call it to stand in for a tailnet connection.
func WithTailnetPeer(ctx context.Context, remoteAddr string) context.Context {
	return context.WithValue(ctx, tailnetPeerKey{}, remoteAddr)
}

// TailnetPeerFromContext returns the remote address [TailnetConnContext]
// attached, and false when the request did not arrive over the tailnet.
func TailnetPeerFromContext(ctx context.Context) (string, bool) {
	addr, ok := ctx.Value(tailnetPeerKey{}).(string)
	return addr, ok
}

// WhoIs answers which tailnet node holds the remote address of a
// connection, as the node's stable id. The tailnet transport provides one
// over tsnet's local client; specs provide a table.
type WhoIs interface {
	WhoIs(ctx context.Context, remoteAddr string) (nodeID string, err error)
}

// WhoIsFunc adapts a function to [WhoIs].
type WhoIsFunc func(ctx context.Context, remoteAddr string) (string, error)

// WhoIs implements [WhoIs].
func (f WhoIsFunc) WhoIs(ctx context.Context, remoteAddr string) (string, error) {
	return f(ctx, remoteAddr)
}

// TailscaleResolver identifies a caller on the tailnet by its node.
//
// The identity value is the node's stable id, which survives the node being
// renamed, re-keyed, or given a new address, and which `tailscale status
// --json` reports as the node's ID. A node is a machine, so every process on
// a laptop is one identity and therefore one principal, as every process
// under one uid is over the socket. The person owning the laptop and a uid
// on the service's host is one principal holding both identities.
type TailscaleResolver struct {
	WhoIs WhoIs
}

// Scheme implements [Resolver].
func (TailscaleResolver) Scheme() string { return SchemeTailscale }

// Resolve implements [Resolver]. A request that did not arrive over the
// tailnet is declined. One that did but whose node WhoIs cannot name fails
// the request: the tailnet vouched for the connection, so an answer it
// cannot give is an error, not an anonymous caller.
func (r TailscaleResolver) Resolve(ctx context.Context, _ connect.Peer, _ http.Header) (Identity, error) {
	addr, ok := TailnetPeerFromContext(ctx)
	if !ok {
		return Identity{}, ErrNoIdentity
	}
	if r.WhoIs == nil {
		return Identity{}, errors.New("interceptor: tailnet transport has no WhoIs")
	}
	nodeID, err := r.WhoIs.WhoIs(ctx, addr)
	if err != nil {
		return Identity{}, fmt.Errorf("interceptor: who is %s: %w", addr, err)
	}
	if nodeID == "" {
		return Identity{}, fmt.Errorf("interceptor: who is %s: no node", addr)
	}
	return Identity{Scheme: SchemeTailscale, Value: nodeID}, nil
}
