package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
)

// The reaper is the expiry timer: `skift reap`, run by skift-reap.timer,
// releasing every lease across the deployment whose TTL has passed. It is
// the one write not made on a client's behalf, so it acts as a service
// principal of its own in each organization, and creates that principal
// the first time it runs there. See the Lease section of
// docs/domain-model.md and the Deployment section of docs/architecture.md.

// ReaperIdentityScheme is the identity scheme the reaper's principal is
// found under. The reaper reaches the database from inside the service
// rather than over a transport, and its events say so.
const ReaperIdentityScheme = "service"

// reaperIdentity is the identity value of the reaper's principal in one
// organization. Identities are unique across the deployment while a
// principal belongs to one organization, so the value carries the
// organization.
func reaperIdentity(orgID string) string {
	return "reaper/" + orgID
}

// Reaped is what one run of the reaper did in one organization.
type Reaped struct {
	OrgID string `json:"org_id"`
	// Expired is every lease the run released, oldest expiry first.
	Expired []store.Lease `json:"expired"`
}

// Reap releases every lease in every organization whose TTL has passed,
// as expired, recording each as an event on its node. It is idempotent: a
// lease is released once, and a run that finds nothing writes nothing.
// Two runs at once are safe: the releasing statement takes the row lock
// and re-checks that the lease is still unreleased, so a lease the other
// run released first is skipped rather than released twice, and only the
// run that released it writes the event.
//
// Each organization is reaped in pages of at most pageSize leases, each
// page in its own transaction, so a backlog of expired leases never holds
// one transaction open for long. Zero pageSize takes the page ceiling. An
// organization that fails stops the run with its error; the ones before
// it stay reaped.
func Reap(ctx context.Context, s *store.Store, pol *policy.Bundle, pageSize int) ([]Reaped, error) {
	var orgs []store.Organization
	err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		orgs, err = tx.ListOrganizations(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}

	out := make([]Reaped, 0, len(orgs))
	for _, org := range orgs {
		reaped, err := reapOrganization(ctx, s, pol, org.ID, pageSize)
		if err != nil {
			return out, fmt.Errorf("organization %s: %w", org.ID, err)
		}
		out = append(out, reaped)
	}
	return out, nil
}

// Reap releases every expired lease in every organization. See [Reap].
func (g *Service) Reap(ctx context.Context, pageSize int) ([]Reaped, error) {
	return Reap(ctx, g.s, g.pol, pageSize)
}

// reapOrganization runs [ExpireLeases] for one organization until a page
// releases nothing. Each page releases at least one lease or ends the
// loop, so it terminates however many reapers run.
func reapOrganization(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, pageSize int) (Reaped, error) {
	reaped := Reaped{OrgID: orgID, Expired: []store.Lease{}}
	actor, err := reaperActor(ctx, s, orgID)
	if err != nil {
		return reaped, err
	}
	for {
		expired, err := ExpireLeases(ctx, s, pol, orgID, actor, pageSize)
		if err != nil {
			return reaped, err
		}
		if len(expired) == 0 {
			return reaped, nil
		}
		reaped.Expired = append(reaped.Expired, expired...)
	}
}

// reaperActor is the reaper's service principal in the organization,
// created on first use. Two reapers creating it at once race on the
// identity's primary key; the loser rolls back and reads the winner's.
func reaperActor(ctx context.Context, s *store.Store, orgID string) (Actor, error) {
	scheme, value := ReaperIdentityScheme, reaperIdentity(orgID)
	for attempt := 0; ; attempt++ {
		var p store.Principal
		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
			p, err = tx.GetPrincipalByIdentity(ctx, store.GetPrincipalByIdentityParams{Scheme: scheme, Value: value})
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("look up reaper principal: %w", err)
			}
			p, err = tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: orgID, Kind: "service"})
			if err != nil {
				return fmt.Errorf("create reaper principal: %w", err)
			}
			if _, err := tx.AddPrincipalIdentity(ctx, store.AddPrincipalIdentityParams{OrgID: orgID, ID: p.ID, Scheme: scheme, Value: value}); err != nil {
				return fmt.Errorf("create reaper identity: %w", err)
			}
			return nil
		})
		if isIdentityConflict(err) && attempt == 0 {
			continue
		}
		if err != nil {
			return Actor{}, err
		}
		if p.OrgID != orgID {
			return Actor{}, fmt.Errorf("reaper identity %s:%s belongs to organization %s", scheme, value, p.OrgID)
		}
		return Actor{PrincipalID: p.ID, PrincipalKind: p.Kind, IdentityScheme: scheme}, nil
	}
}

// isIdentityConflict reports a unique violation on principal identities:
// another reaper created the principal between this one's lookup and its
// insert.
func isIdentityConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "principal_identities_pkey"
}
