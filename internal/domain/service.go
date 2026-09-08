package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ivy/dagger/internal/policy"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/store/migrations"
)

// The rows the domain hands back, named here so that a caller outside the
// domain can hold one without importing the store. Only the domain reaches
// the store; see docs/architecture.md and the depguard rule in
// .golangci.yml.
type (
	Node            = store.Node
	Project         = store.Project
	Lease           = store.Lease
	Event           = store.Event
	Edge            = store.Edge
	Comment         = store.Comment
	CommentRevision = store.CommentRevision
	Principal       = store.Principal
	Usage           = store.Usage
	// StoredResult is a result row: what completing a node wrote.
	StoredResult = store.Result
)

// ErrProjectRequired is returned by a read that is scoped to one project
// and was given none.
var ErrProjectRequired = errors.New("a project is required")

// ErrPrincipalNotFound is returned by [Service.PrincipalByIdentity] when no
// principal holds the identity.
var ErrPrincipalNotFound = errors.New("principal not found")

// Migrate brings the database at databaseURL to the current schema. It is
// the store's migrator, reachable from here because only the domain imports
// the store.
func Migrate(ctx context.Context, databaseURL string) error {
	return migrations.Migrate(ctx, databaseURL)
}

// Service is the domain as a handler sees it: every operation the wire
// contract needs, over one database and one policy bundle, with no store
// type in any signature that a caller must name. Each method runs in its
// own transaction, or in the several a lease operation takes.
type Service struct {
	s   *store.Store
	pol *policy.Bundle
	// wake is the change stream's ear on the database, shared by every
	// open stream. Its connection is opened outside the pool on the same
	// configuration, so a stream never holds a pooled connection idle.
	wake *listener
}

// NewService wraps pool and pol. The caller owns the pool and closes it.
func NewService(pool *pgxpool.Pool, pol *policy.Bundle) *Service {
	connConfig := pool.Config().ConnConfig
	return &Service{
		s:   store.FromPool(pool),
		pol: pol,
		wake: newListener(func(ctx context.Context) (*pgx.Conn, error) {
			return pgx.ConnectConfig(ctx, connConfig)
		}),
	}
}

// PrincipalByIdentity returns the principal holding the identity a
// transport resolved, or [ErrPrincipalNotFound]. It is what the principal
// interceptor's lookup is built on.
func (g *Service) PrincipalByIdentity(ctx context.Context, scheme, value string) (*Principal, error) {
	var p store.Principal
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		p, err = tx.GetPrincipalByIdentity(ctx, store.GetPrincipalByIdentityParams{Scheme: scheme, Value: value})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s:%s: %w", scheme, value, ErrPrincipalNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("look up identity %s:%s: %w", scheme, value, err)
	}
	return &p, nil
}

// PatchOutcome is what [Service.Apply] returns: the patch's [Result] with
// the derived state of every node it touched.
type PatchOutcome struct {
	*Result
	// States are Result.Nodes with their derived flags, in the same order.
	States []NodeState
}

// Apply applies p for actor. See [Apply].
func (g *Service) Apply(ctx context.Context, orgID string, actor Actor, p Patch) (*PatchOutcome, error) {
	result, err := Apply(ctx, g.s, g.pol, orgID, actor, p)
	if err != nil {
		return nil, err
	}
	out := &PatchOutcome{Result: result}
	err = g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		out.States, err = annotate(ctx, tx, orgID, result.Nodes)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Explained is [Explain]'s answer with the derived state of every node it
// names: the node itself and each blocker and child among the reasons.
type Explained struct {
	*Explanation
	Derived map[string]Derived
}

// Explain answers why the node ref names is not ready. project is what a
// bare `#7` resolves in, and may be empty when ref carries its own.
func (g *Service) Explain(ctx context.Context, orgID, project, ref string) (*Explained, error) {
	var out *Explained
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		resolved, err := resolveNode(ctx, tx, orgID, project, ref)
		if err != nil {
			return err
		}
		e, err := Explain(ctx, tx, orgID, resolved.ID)
		if err != nil {
			return err
		}
		nodes := []store.Node{e.Node}
		for _, r := range e.Reasons {
			if r.Blocker != nil {
				nodes = append(nodes, *r.Blocker)
			}
			if r.Child != nil {
				nodes = append(nodes, *r.Child)
			}
		}
		derived, err := derive(ctx, tx, orgID, nodes)
		if err != nil {
			return err
		}
		out = &Explained{Explanation: e, Derived: derived}
		return nil
	})
	return out, err
}

// GetInputs returns the predecessors that satisfy the node ref names, for
// actor. See [ReadInputs].
func (g *Service) GetInputs(ctx context.Context, orgID string, actor Actor, project, ref string) ([]InputState, error) {
	var out []InputState
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		out, err = ReadInputs(ctx, tx, g.pol, orgID, actor, project, ref)
		return err
	})
	return out, err
}

// Search runs q for actor. See [Search].
func (g *Service) Search(ctx context.Context, orgID string, actor Actor, q SearchQuery) ([]Hit, error) {
	var hits []Hit
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		hits, err = Search(ctx, tx, g.pol, orgID, actor, q)
		return err
	})
	return hits, err
}

// Ready returns one page of the ready set of the project ref names, most
// urgent and then oldest first. q.Projects is replaced by the resolved
// project. See [Ready].
func (g *Service) Ready(ctx context.Context, orgID, project string, q ReadyQuery) ([]NodeState, int, error) {
	if project == "" {
		return nil, 0, ErrProjectRequired
	}
	var out []NodeState
	var held int
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		p, err := resolveProject(ctx, tx, orgID, project)
		if err != nil {
			return err
		}
		q.Projects = []string{p.ID}
		nodes, err := Ready(ctx, tx, orgID, q)
		if err != nil {
			return err
		}
		// An empty first page owes its caller the difference between
		// finished and parked, and nothing else has to pay for the
		// count.
		if len(nodes) == 0 && q.Offset == 0 {
			if held, err = HeldCount(ctx, tx, orgID, p.ID); err != nil {
				return err
			}
		}
		out, err = annotate(ctx, tx, orgID, nodes)
		return err
	})
	return out, held, err
}

// recorder is the event writer for one transaction, carrying the actor,
// the policy and the request every event of that transaction shares.
func (g *Service) recorder(tx store.Tx, orgID string, actor Actor, requestID *string) *recorder {
	return &recorder{tx: tx, orgID: orgID, actor: actor, policy: g.pol, request: requestID}
}

// ClaimOutcome is what a claim produced, with the claimed node's derived
// state. Node is nil when the wait elapsed with nothing to claim.
type ClaimOutcome struct {
	*Claimed
	Node *NodeState
	// Inputs is how much the claimed node's inputs come to, so the
	// claimant knows they exist without being handed them.
	Inputs InputsHint
}

// ClaimNext claims the next ready node matching req, waiting up to
// req.Wait. See [ClaimNext].
func (g *Service) ClaimNext(ctx context.Context, orgID string, actor Actor, req ClaimRequest) (*ClaimOutcome, error) {
	claimed, err := ClaimNext(ctx, g.s, g.pol, orgID, actor, req)
	if err != nil || claimed == nil {
		return nil, err
	}
	out := &ClaimOutcome{Claimed: claimed}
	err = g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		states, err := annotate(ctx, tx, orgID, []store.Node{claimed.Node})
		if err != nil {
			return err
		}
		out.Node = &states[0]
		out.Inputs, err = InputsSize(ctx, tx, orgID, claimed.Node.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Heartbeat renews a lease. See [Heartbeat].
func (g *Service) Heartbeat(ctx context.Context, orgID string, actor Actor, requestID, leaseID string, ttl time.Duration) (*LeaseChange, error) {
	return Heartbeat(ctx, g.s, g.pol, orgID, actor, requestID, leaseID, ttl)
}

// Release ends a lease. See [Release].
func (g *Service) Release(ctx context.Context, orgID string, actor Actor, requestID, leaseID, reason, detail string) (*LeaseChange, error) {
	return Release(ctx, g.s, g.pol, orgID, actor, requestID, leaseID, reason, detail)
}

// resolveNode resolves ref, reading a bare `#7` in project when one is
// given. Outside a patch there are no aliases to resolve.
func resolveNode(ctx context.Context, tx store.Tx, orgID, project, ref string) (Resolved, error) {
	var p *store.Project
	if project != "" {
		resolved, err := resolveProject(ctx, tx, orgID, project)
		if err != nil {
			return Resolved{}, err
		}
		p = resolved
	}
	return NewResolver(tx, orgID, p).Resolve(ctx, ref)
}
