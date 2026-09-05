package interceptor

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"connectrpc.com/connect"
)

// SchemeUnix is the identity scheme of callers on a unix socket.
const SchemeUnix = "unix"

// PeerCredentials are what the kernel reports about the process at the
// other end of a unix socket.
type PeerCredentials struct {
	UID int
	GID int
	PID int
}

type peerCredentialsKey struct{}

// UnixConnContext is an http.Server ConnContext. When conn is a unix socket
// connection it reads the peer's credentials with SO_PEERCRED and stores
// them on the context every request on that connection inherits. Other
// connections, and a socket the kernel will not report on, pass through
// untouched, so [UnixResolver] declines them.
func UnixConnContext(ctx context.Context, conn net.Conn) context.Context {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return ctx
	}
	creds, err := peerCredentials(uc)
	if err != nil {
		return ctx
	}
	return WithPeerCredentials(ctx, creds)
}

// WithPeerCredentials returns a context carrying creds as if they had been
// read from the connection. Specs use it to stand in for a caller under
// another uid.
func WithPeerCredentials(ctx context.Context, creds PeerCredentials) context.Context {
	return context.WithValue(ctx, peerCredentialsKey{}, creds)
}

// PeerCredentialsFromContext returns the credentials [UnixConnContext]
// attached, and false when the request did not arrive over a unix socket.
func PeerCredentialsFromContext(ctx context.Context) (PeerCredentials, bool) {
	creds, ok := ctx.Value(peerCredentialsKey{}).(PeerCredentials)
	return creds, ok
}

// UnixResolver identifies a caller on a unix socket by its uid.
//
// SO_PEERCRED reports the unix user, not the process's purpose, so every
// process one user runs is the same identity and therefore the same
// principal. That is how agent identity is modelled: a host and a unix
// user, with agents sharing a user sharing a principal. The identity value
// is the decimal uid; the host is implied, since a unix socket only ever
// reaches processes on the machine the service runs on.
type UnixResolver struct{}

// Scheme implements [Resolver].
func (UnixResolver) Scheme() string { return SchemeUnix }

// Resolve implements [Resolver].
func (UnixResolver) Resolve(ctx context.Context, _ connect.Peer, _ http.Header) (Identity, error) {
	creds, ok := PeerCredentialsFromContext(ctx)
	if !ok {
		return Identity{}, ErrNoIdentity
	}
	return Identity{Scheme: SchemeUnix, Value: strconv.Itoa(creds.UID)}, nil
}
