package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steading-ai/skift/internal/store"
)

// No RPC creates an organization, a principal, or an identity: a caller
// has to be one before it can call anything. Bootstrap is the one write
// that starts from nothing, run by a person or an installer against the
// database directly, before the service has anyone to serve.

// The identity schemes of a caller on the unix socket and on the tailnet,
// and the kind of the principal bootstrap creates. All are spelled here
// rather than imported from the server package, which the domain does not
// depend on.
const (
	bootstrapScheme    = "unix"
	bootstrapTailscale = "tailscale"
	bootstrapKind      = "user"
)

// bootstrapLock is the advisory lock key two bootstraps of one database
// serialize on, so that a race creates one organization rather than two
// carrying the same name.
const bootstrapLock = 0x736b6966740001 // "skift" then 1

// ErrIdentityElsewhere is returned when the identity bootstrap would bind
// already belongs to a principal of another organization. Bootstrap does
// not move it; that is a decision for a person.
var ErrIdentityElsewhere = errors.New("identity belongs to a principal of another organization")

// ErrIdentityHeld is returned when the tailscale identity bootstrap would
// bind to the uid's principal already belongs to a different principal.
// Bootstrap does not move it either.
var ErrIdentityHeld = errors.New("identity belongs to another principal")

// BootstrapParams names what the first organization and its first
// principal are.
type BootstrapParams struct {
	// OrgName is the organization's name. An organization carrying it
	// already is adopted; otherwise one is created.
	OrgName string
	// UID is the numeric unix user id the socket will report for the
	// principal. A principal holding it already is adopted.
	UID uint32
	// TailscaleNode, when set, is the stable id of a tailnet node the same
	// principal is reached from, which the tailnet transport reports for
	// it. It is bound to the principal the uid resolves to; a node already
	// bound there is adopted.
	TailscaleNode string
}

// Bootstrapped is what [Bootstrap] left in the database, whether it
// created it or found it there.
type Bootstrapped struct {
	Organization BootstrappedOrganization `json:"organization"`
	Principal    BootstrappedPrincipal    `json:"principal"`
}

// BootstrappedOrganization is the organization the principal belongs to.
type BootstrappedOrganization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Created is whether this run created it, as opposed to finding it.
	Created bool `json:"created"`
}

// BootstrappedPrincipal is the principal bound to the unix identity.
type BootstrappedPrincipal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Identities are the scheme and value pairs the transports resolve to
	// it: the unix one always, the tailscale one when a node was given.
	Identities []BootstrappedIdentity `json:"identities"`
	// Created is whether this run created it, as opposed to finding it.
	Created bool `json:"created"`
}

// BootstrappedIdentity is one row of principal_identities.
type BootstrappedIdentity struct {
	Scheme string `json:"scheme"`
	Value  string `json:"value"`
	// Created is whether this run bound it, as opposed to finding it.
	Created bool `json:"created"`
}

// Bootstrap ensures the database holds an organization named
// params.OrgName and, in it, a user principal that a caller with unix uid
// params.UID resolves to, and that a caller from tailnet node
// params.TailscaleNode, when one is given, resolves to as well. It is
// idempotent: a second run with the same
// arguments changes nothing and reports the rows the first left, so an
// installer can run it without knowing whether it has run before. Two
// runs at once serialize on an advisory lock and reach the same result.
//
// An identity held by a principal of another organization is not moved:
// that fails with [ErrIdentityElsewhere] and the transaction rolls back.
// Nor is a tailscale identity held by any other principal; that fails with
// [ErrIdentityHeld].
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, params BootstrapParams) (Bootstrapped, error) {
	if params.OrgName == "" {
		return Bootstrapped{}, errors.New("bootstrap: an organization name is required")
	}
	identity := BootstrappedIdentity{Scheme: bootstrapScheme, Value: strconv.FormatUint(uint64(params.UID), 10)}
	var out Bootstrapped
	err := store.FromPool(pool).WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.Tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLock); err != nil {
			return fmt.Errorf("lock bootstrap: %w", err)
		}

		org, err := tx.GetOrganizationByName(ctx, params.OrgName)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			org, err = tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: newULID(), Name: params.OrgName})
			if err != nil {
				return fmt.Errorf("create organization: %w", err)
			}
			out.Organization.Created = true
		case err != nil:
			return fmt.Errorf("look up organization: %w", err)
		}
		out.Organization.ID, out.Organization.Name = org.ID, org.Name

		principal, err := tx.GetPrincipalByIdentity(ctx, store.GetPrincipalByIdentityParams{Scheme: identity.Scheme, Value: identity.Value})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			principal, err = tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: org.ID, Kind: bootstrapKind})
			if err != nil {
				return fmt.Errorf("create principal: %w", err)
			}
			if _, err := tx.AddPrincipalIdentity(ctx, store.AddPrincipalIdentityParams{
				OrgID: org.ID, ID: principal.ID, Scheme: identity.Scheme, Value: identity.Value,
			}); err != nil {
				return fmt.Errorf("bind identity: %w", err)
			}
			out.Principal.Created, identity.Created = true, true
		case err != nil:
			return fmt.Errorf("look up identity: %w", err)
		case principal.OrgID != org.ID:
			return fmt.Errorf("%s:%s: %w", identity.Scheme, identity.Value, ErrIdentityElsewhere)
		}
		out.Principal.ID, out.Principal.Kind = principal.ID, principal.Kind
		out.Principal.Identities = append(out.Principal.Identities, identity)

		if params.TailscaleNode == "" {
			return nil
		}
		node := BootstrappedIdentity{Scheme: bootstrapTailscale, Value: params.TailscaleNode}
		holder, err := tx.GetPrincipalByIdentity(ctx, store.GetPrincipalByIdentityParams{Scheme: node.Scheme, Value: node.Value})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.AddPrincipalIdentity(ctx, store.AddPrincipalIdentityParams{
				OrgID: org.ID, ID: principal.ID, Scheme: node.Scheme, Value: node.Value,
			}); err != nil {
				return fmt.Errorf("bind tailscale identity: %w", err)
			}
			node.Created = true
		case err != nil:
			return fmt.Errorf("look up tailscale identity: %w", err)
		case holder.ID != principal.ID:
			return fmt.Errorf("%s:%s: %w", node.Scheme, node.Value, ErrIdentityHeld)
		}
		out.Principal.Identities = append(out.Principal.Identities, node)
		return nil
	})
	if err != nil {
		return Bootstrapped{}, err
	}
	return out, nil
}
