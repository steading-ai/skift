// Package interceptor holds the Connect interceptors that sit between the
// transport and the handlers. See the request path in docs/architecture.md.
//
// The first of them resolves the caller. A [Resolver] per transport turns
// the connection into an [Identity]; a [Lookup] turns that identity into the
// [Principal] it belongs to; both land in the request context. Handlers read
// them with [PrincipalFromContext] and [IdentityFromContext] and never learn
// which transport the request arrived over. Adding a transport is adding a
// Resolver, and no handler changes. See the Principal section of
// docs/domain-model.md and docs/adrs/002-connect-rpc-is-the-only-door.md.
//
// Interceptors authenticate and check what a principal may call, and
// nothing finer. Which resources a request touches is unknown here: a
// patch's aliases resolve and its cascades expand only when the domain
// applies it, so resource-level authorization is the domain's, inside the
// writing transaction. See the request path in docs/architecture.md.
package interceptor

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
)

// Kind is what sort of actor a principal is. The values match the
// principals.kind column and Principal.kind in the domain model.
type Kind string

// The three kinds of principal.
const (
	KindUser    Kind = "user"
	KindAgent   Kind = "agent"
	KindService Kind = "service"
)

// Principal is the stable actor every event and lease refers to.
type Principal struct {
	// ID is the principal's ULID.
	ID string
	// OrgID is the organization the principal belongs to.
	OrgID string
	Kind  Kind
}

// Identity is how a transport recognised the caller: one row of
// principal_identities, keyed by scheme and value. Several identities may
// reach one principal.
type Identity struct {
	// Scheme names the transport, such as [SchemeUnix]. It is what events
	// record as identity_scheme.
	Scheme string
	// Value is the transport's name for the caller, in the scheme's own
	// terms.
	Value string
}

func (id Identity) String() string {
	return id.Scheme + ":" + id.Value
}

// Resolver identifies the caller of one request from its transport. One
// exists per transport, and the principal interceptor tries them in order.
type Resolver interface {
	// Scheme is the identity scheme this resolver produces.
	Scheme() string
	// Resolve returns the caller's identity. It returns [ErrNoIdentity]
	// when the request did not arrive over this resolver's transport, so
	// the next resolver is tried; any other error fails the request.
	Resolve(ctx context.Context, peer connect.Peer, header http.Header) (Identity, error)
}

// ErrNoIdentity is returned by a Resolver whose transport carries nothing it
// recognises for this request.
var ErrNoIdentity = errors.New("interceptor: no identity on this transport")

// Lookup maps an identity to its principal. The domain layer provides one
// over principal_identities; [StaticLookup] is one for specs and fixtures.
type Lookup interface {
	// LookupIdentity returns the principal holding id, or an
	// [*UnmappedIdentityError] when no principal does.
	LookupIdentity(ctx context.Context, id Identity) (Principal, error)
}

// UnmappedIdentityError is the typed failure for an identity the transport
// resolved but no principal holds. The interceptor reports it to the caller
// as Unauthenticated.
type UnmappedIdentityError struct {
	Identity Identity
}

func (e *UnmappedIdentityError) Error() string {
	return fmt.Sprintf("interceptor: identity %s maps to no principal", e.Identity)
}

// StaticLookup is a Lookup over a fixed table.
type StaticLookup map[Identity]Principal

// LookupIdentity implements [Lookup].
func (s StaticLookup) LookupIdentity(_ context.Context, id Identity) (Principal, error) {
	p, ok := s[id]
	if !ok {
		return Principal{}, &UnmappedIdentityError{Identity: id}
	}
	return p, nil
}

// caller is what the interceptor places in the request context.
type caller struct {
	principal Principal
	identity  Identity
}

type callerKey struct{}

// WithCaller returns a context attributing requests to principal, reached
// through identity. The interceptor calls it; handler specs call it to stand
// in for a transport.
func WithCaller(ctx context.Context, principal Principal, identity Identity) context.Context {
	return context.WithValue(ctx, callerKey{}, caller{principal: principal, identity: identity})
}

// PrincipalFromContext returns the resolved principal of the request, and
// false when the request was not attributed. A handler behind the principal
// interceptor always finds one.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	c, ok := ctx.Value(callerKey{}).(caller)
	return c.principal, ok
}

// IdentityFromContext returns the identity the principal was resolved
// through. Its Scheme is what an event records as identity_scheme.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	c, ok := ctx.Value(callerKey{}).(caller)
	return c.identity, ok
}

// principalInterceptor is the resolution step of the request path.
type principalInterceptor struct {
	lookup    Lookup
	resolvers []Resolver
}

// NewPrincipal returns the interceptor that attributes every request to a
// principal. Resolvers are consulted in order; the first that recognises the
// transport decides the identity. A request no resolver recognises, or whose
// identity no principal holds, fails with [connect.CodeUnauthenticated]
// before reaching a handler.
func NewPrincipal(lookup Lookup, resolvers ...Resolver) connect.Interceptor {
	return &principalInterceptor{lookup: lookup, resolvers: resolvers}
}

// resolve runs the resolvers and the lookup and returns the attributed
// context.
func (p *principalInterceptor) resolve(ctx context.Context, peer connect.Peer, header http.Header) (context.Context, error) {
	for _, r := range p.resolvers {
		id, err := r.Resolve(ctx, peer, header)
		if errors.Is(err, ErrNoIdentity) {
			continue
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("%s: %w", r.Scheme(), err))
		}
		principal, err := p.lookup.LookupIdentity(ctx, id)
		if err != nil {
			var unmapped *UnmappedIdentityError
			if errors.As(err, &unmapped) {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("interceptor: look up %s: %w", id, err))
		}
		return WithCaller(ctx, principal, id), nil
	}
	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("interceptor: no transport identity"))
}

// WrapUnary implements [connect.Interceptor].
func (p *principalInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := p.resolve(ctx, req.Peer(), req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient implements [connect.Interceptor]. Resolution is a
// server concern, so the client side passes through.
func (p *principalInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements [connect.Interceptor].
func (p *principalInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := p.resolve(ctx, conn.Peer(), conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
