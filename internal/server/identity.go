package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/server/interceptor"
)

// IdentityLookup is the [interceptor.Lookup] over principal_identities,
// through the domain.
type IdentityLookup struct {
	svc *domain.Service
}

// NewIdentityLookup returns the lookup the principal interceptor uses in
// front of svc's handlers.
func NewIdentityLookup(svc *domain.Service) *IdentityLookup {
	return &IdentityLookup{svc: svc}
}

// LookupIdentity implements [interceptor.Lookup].
func (l *IdentityLookup) LookupIdentity(ctx context.Context, id interceptor.Identity) (interceptor.Principal, error) {
	p, err := l.svc.PrincipalByIdentity(ctx, id.Scheme, id.Value)
	if errors.Is(err, domain.ErrPrincipalNotFound) {
		return interceptor.Principal{}, &interceptor.UnmappedIdentityError{Identity: id}
	}
	if err != nil {
		return interceptor.Principal{}, fmt.Errorf("server: %w", err)
	}
	return interceptor.Principal{ID: p.ID, OrgID: p.OrgID, Kind: interceptor.Kind(p.Kind)}, nil
}

// caller is the transport-resolved actor of the request and the
// organization it acts in. A request the principal interceptor did not
// attribute is refused as unauthenticated: a handler never invents an actor
// and never reads one from a request body. See
// docs/adrs/006-provenance-not-authority.md.
func caller(ctx context.Context) (orgID string, actor domain.Actor, err error) {
	p, ok := interceptor.PrincipalFromContext(ctx)
	if !ok {
		return "", domain.Actor{}, connectError(domain.ErrNoActor)
	}
	id, _ := interceptor.IdentityFromContext(ctx)
	return p.OrgID, domain.Actor{PrincipalID: p.ID, PrincipalKind: string(p.Kind), IdentityScheme: id.Scheme}, nil
}
