package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/steading-ai/skift/internal/policy"
	"github.com/steading-ai/skift/internal/store"
)

// A lease is the contract between skift and whatever executes work: one
// attempt on one node, bounded by a TTL and fenced by its id. Claim returns
// the id, and every later write to the node carries it. The server checks,
// in the statement that writes, that the lease is the node's current one
// and has not expired, so a worker whose lease was reclaimed cannot land a
// late result on work someone else now owns. See the Lease section of
// docs/domain-model.md, including Fencing.

// TTL and wait bounds. These are fixed ceilings rather than tunables: an
// unbounded TTL defeats expiry, and an unbounded wait is a connection held
// forever. See the Limits section of docs/domain-model.md.
const (
	// DefaultLeaseTTL applies when a claim or heartbeat requests none, and
	// is what a write by the holder renews the lease to.
	DefaultLeaseTTL = 15 * time.Minute
	// MaxClaimWait is the ceiling a ClaimNext wait is clamped to.
	MaxClaimWait = 5 * time.Minute
)

// Between polls a waiting ClaimNext holds nothing: no transaction, no row
// lock, no connection. It sleeps, then asks again. The interval starts
// short so fresh work is picked up promptly and backs off so idle
// orchestrators cost little.
const (
	claimPollInitial = 100 * time.Millisecond
	claimPollMax     = time.Second
)

// claimAttempts bounds how many times one claim restarts after losing a
// race for a node. Each restart takes a fresh snapshot, so losing repeatedly
// means the ready set is being drained faster than this claimant can read
// it, which is contention worth reporting rather than hiding.
const claimAttempts = 8

// Release reasons, as a lease records why it ended.
const (
	// ReleaseComplete: the holder completed the node.
	ReleaseComplete = "complete"
	// ReleaseGate: the holder filed a gate and stepped back to wait on it.
	ReleaseGate = "gate"
	// ReleaseCrash: the attempt failed.
	ReleaseCrash = "crash"
	// ReleaseBudget: the holder stopped at a spending limit.
	ReleaseBudget = "budget"
	// ReleaseExpired: the TTL passed without renewal. Set by the server
	// alone; a client cannot release with it.
	ReleaseExpired = "expired"
	// ReleasePreempted: the lease was taken away.
	ReleasePreempted = "preempted"
)

// Lease operation names, as written to events.op.
const (
	OpClaimNode      = "claim_node"
	OpHeartbeatLease = "heartbeat_lease"
	OpReleaseLease   = "release_lease"
	OpExpireLease    = "expire_lease"
)

// Errors a lease operation, or a fenced write, is rejected with.
var (
	// ErrLeaseNotLive: the lease id names no lease that is unreleased and
	// within its TTL. A stale id and an expired lease are the same
	// rejection: neither may write.
	ErrLeaseNotLive = errors.New("lease is not live")
	// ErrNotLeaseHolder: the lease is live but belongs to another
	// principal.
	ErrNotLeaseHolder = errors.New("lease is held by another principal")
	// ErrLeaseRequired: the node is held by a live lease and the write
	// carries no lease id.
	ErrLeaseRequired = errors.New("node is held by a lease and the write carries none")
	// ErrLeaseMismatch: the node is held by a live lease other than the
	// one the write carries. The write's lease was reclaimed.
	ErrLeaseMismatch = errors.New("lease is not the node's current lease")
	// ErrNegativeDuration: a TTL or wait below zero.
	ErrNegativeDuration = errors.New("duration is negative")
	// ErrClaimContended: the claim lost the race for a node more times
	// than it will retry.
	ErrClaimContended = errors.New("claim contended")
	// ErrNoLeaseID: a lease operation names no lease.
	ErrNoLeaseID = errors.New("a lease id is required")
)

// ClaimRequest asks for the next ready node of one project matching the
// filters, claimed. Its JSON form, with Wait cleared, is what the
// idempotency hash is taken over: how long the client was willing to wait
// is not part of what it asked for.
type ClaimRequest struct {
	// RequestID is the client-supplied idempotency key.
	RequestID string `json:"request_id"`
	// Projects restricts the claim to these projects: each a slug,
	// `org/slug`, or an id. Empty means every project of the organization,
	// and the claim rotates among whichever of them have ready work, one
	// project's turn per successful claim. See the Priority section of
	// docs/domain-model.md.
	Projects []string `json:"projects,omitempty"`
	// Kinds, AssigneeKind, Labels, AnyLabels, MinPriority, and Parent
	// filter the ready set as [ReadyQuery] does.
	Kinds        []string `json:"kinds,omitempty"`
	AssigneeKind string   `json:"assignee_kind,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	AnyLabels    []string `json:"any_labels,omitempty"`
	MinPriority  string   `json:"min_priority,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	// TTL is the requested lease TTL. Zero takes [DefaultLeaseTTL]; more
	// than [MaxLeaseTTL] is clamped to it.
	TTL time.Duration `json:"ttl,omitempty"`
	// Harness names the executing harness, such as claude-code or hive.
	Harness string `json:"harness"`
	// AgentRef is where this attempt will run, recorded for placement.
	AgentRef string `json:"agent_ref"`
	// SessionRef is the harness session, when one works several nodes.
	SessionRef string `json:"session_ref,omitempty"`
	// Wait is how long [ClaimNext] keeps asking before returning empty.
	// Zero asks once. More than [MaxClaimWait] is clamped to it.
	Wait time.Duration `json:"wait,omitempty"`
	// Stop, when it closes, ends the wait early between asks with an
	// empty answer, never inside a transaction. A server that is draining
	// closes it. Nil never fires.
	Stop <-chan struct{} `json:"-"`
}

func (r ClaimRequest) validate(actor Actor) error {
	if r.RequestID == "" {
		return ErrNoRequestID
	}
	if err := actor.validate(); err != nil {
		return err
	}
	if r.TTL < 0 || r.Wait < 0 {
		return ErrNegativeDuration
	}
	return nil
}

// Claimed is what a claim produced: the node, the lease fencing it, and the
// events the claim wrote. It is what the idempotency key stores, so a
// retried claim gets the same node and lease back rather than a second
// node.
type Claimed struct {
	Node  store.Node  `json:"node"`
	Lease store.Lease `json:"lease"`
	// Events are the events the claim wrote, in order: an expiry for a
	// lease whose TTL had passed, then the claim.
	Events []store.Event `json:"events"`
	// Replayed reports that this is the stored response of an earlier
	// application of the same request, and nothing was written.
	Replayed bool `json:"-"`
}

// LeaseChange is what a heartbeat or a release produced: the lease as it
// stands after, and the event recording it.
type LeaseChange struct {
	Lease  store.Lease   `json:"lease"`
	Events []store.Event `json:"events"`
	// Replayed reports that this is the stored response of an earlier
	// application of the same request, and nothing was written.
	Replayed bool `json:"-"`
}

// boundTTL applies the default and the ceiling to a requested TTL. Over
// the ceiling is clamped rather than refused: the holder can always renew,
// so a long request loses nothing but the guarantee it was after.
func boundTTL(ttl time.Duration) time.Duration {
	if ttl == 0 {
		return DefaultLeaseTTL
	}
	return min(ttl, ActiveLimits().LeaseTTL)
}

// Claim takes the most urgent ready node of the request's project that
// matches its filters, or returns nil when there is none right now. The
// node is selected and locked with `FOR UPDATE SKIP LOCKED`, so concurrent
// claimants take different nodes rather than waiting on one another, and a
// node can carry at most one live lease however many claim it at once.
//
// A lease whose TTL has passed but which the expiry timer has not yet
// released does not keep the node out of the ready set; the claim releases
// it as expired first, and records that as an event, so correctness does
// not wait on the timer.
//
// A request id seen before with the same content returns the stored
// [Claimed] with Replayed set and claims nothing more. Returning empty
// stores nothing, so a retry after an empty response can still claim.
func Claim(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, req ClaimRequest) (*Claimed, error) {
	if err := req.validate(actor); err != nil {
		return nil, err
	}
	hashed := req
	hashed.Wait = 0
	hash, err := requestHash(hashed)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < claimAttempts; attempt++ {
		var claimed *Claimed
		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
			claimed, err = claimOnce(ctx, tx, pol, orgID, actor, req, hash)
			return err
		})
		switch {
		case errors.Is(err, errNothingReady):
			return nil, nil
		case errors.Is(err, errClaimLost), isKeyConflict(err), isLiveLeaseConflict(err):
			continue
		case err != nil:
			return nil, auditDenial(ctx, s, orgID, actor, &req.RequestID, err)
		}
		return claimed, nil
	}
	return nil, ErrClaimContended
}

// errNothingReady rolls a claim back when the ready set is empty, so the
// request id is not consumed by an empty answer.
var errNothingReady = errors.New("nothing ready")

// errClaimLost rolls a claim back when the node it locked turned out, on a
// fresh look, to have been taken or blocked by a transaction that committed
// between the claim's snapshot and its lock. The claim restarts with a
// fresh snapshot.
var errClaimLost = errors.New("claim lost")

// claimOnce is one attempt at the claim inside tx.
func claimOnce(ctx context.Context, tx store.Tx, pol *policy.Bundle, orgID string, actor Actor, req ClaimRequest, hash string) (*Claimed, error) {
	claimed, replayed, err := withRequest(ctx, tx, orgID, req.RequestID, hash, func(ctx context.Context) (*Claimed, error) {
		scope, err := resolveScope(ctx, tx, orgID, req.Projects)
		if err != nil {
			return nil, err
		}
		q := ReadyQuery{
			Projects: scope, Kinds: req.Kinds, AssigneeKind: req.AssigneeKind,
			Labels: req.Labels, AnyLabels: req.AnyLabels, MinPriority: req.MinPriority,
			Parent: req.Parent, Limit: 1,
		}
		parentID, err := resolveParent(ctx, tx, orgID, q)
		if err != nil {
			return nil, err
		}
		eligible, err := eligibleProjects(ctx, tx, orgID, q, parentID)
		if err != nil {
			return nil, err
		}
		if len(eligible) == 0 {
			return nil, errNothingReady
		}
		cursorHash, err := filterHash(q, parentID)
		if err != nil {
			return nil, err
		}
		cursor, err := tx.GetClaimCursor(ctx, store.GetClaimCursorParams{OrgID: orgID, PrincipalID: actor.PrincipalID, FilterHash: cursorHash})
		var last *string
		if err == nil {
			last = &cursor.ProjectID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read claim cursor: %w", err)
		}
		chosen := nextProject(last, eligible)
		if _, err := tx.UpsertClaimCursor(ctx, store.UpsertClaimCursorParams{
			OrgID: orgID, PrincipalID: actor.PrincipalID, FilterHash: cursorHash, ProjectID: chosen,
		}); err != nil {
			return nil, fmt.Errorf("advance claim cursor: %w", err)
		}

		single := q
		single.Projects = []string{chosen}
		node, err := lockNextReady(ctx, tx, orgID, single, parentID)
		if err != nil {
			return nil, err
		}

		// The row lock is held from here to commit. Read committed gives
		// each statement below a fresh snapshot, so what they see is
		// everything that committed before the lock was taken, which the
		// selecting statement's snapshot may predate.
		r := recorder{tx: tx, orgID: orgID, actor: actor, policy: pol, request: &req.RequestID}
		if err := r.expireOn(ctx, node); err != nil {
			return nil, err
		}
		ready, err := isReady(ctx, tx, orgID, node.ID)
		if err != nil {
			return nil, err
		}
		if !ready {
			return nil, errClaimLost
		}

		lease, err := tx.CreateLease(ctx, store.CreateLeaseParams{
			ID: newULID(), OrgID: orgID, NodeID: node.ID, Holder: actor.PrincipalID,
			Harness: req.Harness, AgentRef: req.AgentRef, SessionRef: optional(req.SessionRef),
			ExpiresAt: time.Now().Add(boundTTL(req.TTL)),
		})
		if err != nil {
			return nil, fmt.Errorf("create lease: %w", err)
		}
		if err := r.event(ctx, OpClaimNode, node.ID, node.ProjectID, nil, nil, lease); err != nil {
			return nil, err
		}
		return &Claimed{Node: node, Lease: lease, Events: r.events}, nil
	})
	if err != nil {
		return nil, err
	}
	claimed.Replayed = replayed
	return claimed, nil
}

// lockNextReady selects the first node of the ordered ready set and locks
// its row, skipping rows other claimants hold. errNothingReady when the set
// is empty.
func lockNextReady(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery, parentID string) (store.Node, error) {
	sql, args, err := q.sql(orgID, true, parentID)
	if err != nil {
		return store.Node{}, err
	}
	rows, err := tx.Tx.Query(ctx, sql+" FOR UPDATE OF n SKIP LOCKED", args...)
	if err != nil {
		return store.Node{}, fmt.Errorf("select next ready node: %w", err)
	}
	node, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[store.Node])
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Node{}, errNothingReady
	}
	if err != nil {
		return store.Node{}, fmt.Errorf("select next ready node: %w", err)
	}
	return node, nil
}

// isReady reports whether one node satisfies the readiness predicate as
// of a fresh statement.
func isReady(ctx context.Context, tx store.Tx, orgID, nodeID string) (bool, error) {
	var ready bool
	err := tx.Tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM nodes n WHERE "+readyWhere+" AND n.id = $2)", orgID, nodeID).Scan(&ready)
	if err != nil {
		return false, fmt.Errorf("recheck readiness: %w", err)
	}
	return ready, nil
}

// isLiveLeaseConflict reports a unique violation on the live-lease index:
// another claim on the same node committed first. The readiness recheck
// under the row lock makes this unreachable in practice; the index is the
// invariant's backstop, and a claim losing to it restarts.
func isLiveLeaseConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "leases_live_idx"
}

// ClaimNext is [Claim] that keeps asking until it has a node or the
// request's Wait has elapsed, when it returns nil. Between asks it holds
// no transaction, row lock, or connection, so any number of orchestrators
// may wait on an empty project without starving the pool. A cancelled
// context ends the wait with its error.
func ClaimNext(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, req ClaimRequest) (*Claimed, error) {
	if err := req.validate(actor); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(min(req.Wait, MaxClaimWait))
	interval := claimPollInitial
	for {
		claimed, err := Claim(ctx, s, pol, orgID, actor, req)
		if err != nil || claimed != nil {
			return claimed, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(min(interval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-req.Stop:
			timer.Stop()
			return nil, nil
		case <-timer.C:
		}
		interval = min(interval*2, claimPollMax)
	}
}

// Heartbeat renews the lease to ttl from now, fenced: the lease must be
// live and held by actor. Zero ttl takes [DefaultLeaseTTL]; more than
// [MaxLeaseTTL] is clamped to it.
func Heartbeat(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, requestID, leaseID string, ttl time.Duration) (*LeaseChange, error) {
	if ttl < 0 {
		return nil, ErrNegativeDuration
	}
	return leaseOp(ctx, s, pol, orgID, actor, requestID, leaseID, OpHeartbeatLease, ttl, func(ctx context.Context, tx store.Tx, r *recorder, before store.Lease, node store.Node) (store.Lease, error) {
		after, err := tx.RenewLease(ctx, store.RenewLeaseParams{OrgID: orgID, ID: before.ID, ExpiresAt: time.Now().Add(boundTTL(ttl))})
		if errors.Is(err, pgx.ErrNoRows) {
			return after, fmt.Errorf("lease %s: %w", before.ID, ErrLeaseNotLive)
		}
		if err != nil {
			return after, fmt.Errorf("renew lease %s: %w", before.ID, err)
		}
		return after, r.event(ctx, OpHeartbeatLease, node.ID, node.ProjectID, nil, before, after)
	})
}

// Release ends the lease with a reason, fenced: the lease must be live and
// held by actor. reason is one of the Release constants other than
// [ReleaseExpired], which only the server sets; detail is free text for
// the event.
func Release(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, requestID, leaseID, reason, detail string) (*LeaseChange, error) {
	switch reason {
	case ReleaseComplete, ReleaseGate, ReleaseCrash, ReleaseBudget, ReleasePreempted:
	case ReleaseExpired:
		return nil, fmt.Errorf("%w: expired is set by the server", ErrReasonRequired)
	case "":
		return nil, ErrReasonRequired
	default:
		return nil, fmt.Errorf("unknown release reason %q", reason)
	}
	eventReason := reason
	if detail != "" {
		eventReason = reason + ": " + detail
	}
	return leaseOp(ctx, s, pol, orgID, actor, requestID, leaseID, OpReleaseLease, eventReason, func(ctx context.Context, tx store.Tx, r *recorder, before store.Lease, node store.Node) (store.Lease, error) {
		after, err := tx.ReleaseLiveLease(ctx, store.ReleaseLiveLeaseParams{OrgID: orgID, ID: before.ID, ReleaseReason: &reason})
		if errors.Is(err, pgx.ErrNoRows) {
			return after, fmt.Errorf("lease %s: %w", before.ID, ErrLeaseNotLive)
		}
		if err != nil {
			return after, fmt.Errorf("release lease %s: %w", before.ID, err)
		}
		return after, r.event(ctx, OpReleaseLease, node.ID, node.ProjectID, &eventReason, before, after)
	})
}

// leaseOp runs one fenced, idempotent write against a lease the actor
// holds. write receives the lease as it stands and its node, and returns
// the lease as it stands after. content distinguishes the request's hash
// from another operation on the same lease under the same request id.
func leaseOp(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, requestID, leaseID, op string, content any, write func(context.Context, store.Tx, *recorder, store.Lease, store.Node) (store.Lease, error)) (*LeaseChange, error) {
	if requestID == "" {
		return nil, ErrNoRequestID
	}
	if leaseID == "" {
		return nil, ErrNoLeaseID
	}
	if err := actor.validate(); err != nil {
		return nil, err
	}
	hash, err := requestHash(struct {
		Op      string `json:"op"`
		LeaseID string `json:"lease_id"`
		Content any    `json:"content"`
	}{op, leaseID, content})
	if err != nil {
		return nil, err
	}

	var change *LeaseChange
	for attempt := 0; ; attempt++ {
		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			result, replayed, err := withRequest(ctx, tx, orgID, requestID, hash, func(ctx context.Context) (*LeaseChange, error) {
				before, err := heldLease(ctx, tx, orgID, actor, leaseID)
				if err != nil {
					return nil, err
				}
				node, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: before.NodeID})
				if err != nil {
					return nil, fmt.Errorf("read node %s: %w", before.NodeID, err)
				}
				r := &recorder{tx: tx, orgID: orgID, actor: actor, policy: pol, request: &requestID, lease: &before}
				after, err := write(ctx, tx, r, before, node)
				if err != nil {
					return nil, err
				}
				return &LeaseChange{Lease: after, Events: r.events}, nil
			})
			if err != nil {
				return err
			}
			result.Replayed = replayed
			change = result
			return nil
		})
		if isKeyConflict(err) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, auditDenial(ctx, s, orgID, actor, &requestID, err)
		}
		return change, nil
	}
}

// heldLease is the lease as it stands when it is live and actor holds it.
// A missing, released, or expired lease is [ErrLeaseNotLive]; another
// principal's is [ErrNotLeaseHolder].
func heldLease(ctx context.Context, tx store.Tx, orgID string, actor Actor, leaseID string) (store.Lease, error) {
	lease, err := tx.GetLiveLease(ctx, store.GetLiveLeaseParams{OrgID: orgID, ID: leaseID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Lease{}, fmt.Errorf("lease %s: %w", leaseID, ErrLeaseNotLive)
	}
	if err != nil {
		return store.Lease{}, fmt.Errorf("read lease %s: %w", leaseID, err)
	}
	if lease.Holder != actor.PrincipalID {
		return store.Lease{}, fmt.Errorf("lease %s: %w", leaseID, ErrNotLeaseHolder)
	}
	return lease, nil
}

// ExpireLeases releases up to pageSize leases of the organization whose
// TTL has passed, as expired, and records each as an event on its node.
// Releasing returns the node to the ready set. This is what the expiry
// timer calls; actor is the service principal it runs as, and the events
// carry no request id since no client asked. The released leases are
// returned, oldest expiry first.
//
// pageSize is bounded by [Limits.PageSize]: zero takes the ceiling, and
// more is a [*LimitError].
func ExpireLeases(ctx context.Context, s *store.Store, pol *policy.Bundle, orgID string, actor Actor, pageSize int) ([]store.Lease, error) {
	if err := actor.validate(); err != nil {
		return nil, err
	}
	pageSize, err := boundPageSize(pageSize, ActiveLimits())
	if err != nil {
		return nil, err
	}
	var expired []store.Lease
	err = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		due, err := tx.ListExpiredLeases(ctx, store.ListExpiredLeasesParams{OrgID: orgID, PageSize: int32(pageSize)})
		if err != nil {
			return fmt.Errorf("list expired leases: %w", err)
		}
		if len(due) == 0 {
			return nil
		}
		ids := make([]string, 0, len(due))
		for _, l := range due {
			ids = append(ids, l.ID)
		}
		r := recorder{tx: tx, orgID: orgID, actor: actor, policy: pol}
		expired, err = r.expire(ctx, due, func() ([]store.Lease, error) {
			return tx.ExpireLeases(ctx, store.ExpireLeasesParams{OrgID: orgID, Ids: ids})
		})
		return err
	})
	if err != nil {
		return nil, auditDenial(ctx, s, orgID, actor, nil, err)
	}
	return expired, nil
}

// expireOn releases the expired lease on node, if there is one, recording
// it. A live lease is left alone.
func (r *recorder) expireOn(ctx context.Context, node store.Node) error {
	unreleased, err := r.tx.GetUnreleasedLease(ctx, store.GetUnreleasedLeaseParams{OrgID: r.orgID, NodeID: node.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lease on node %s: %w", node.ID, err)
	}
	_, err = r.expire(ctx, []store.Lease{unreleased}, func() ([]store.Lease, error) {
		return r.tx.ExpireLeasesForNode(ctx, store.ExpireLeasesForNodeParams{OrgID: r.orgID, NodeID: node.ID})
	})
	return err
}

// expire runs the expiring statement and records one event per lease it
// released, with the lease as listed before and as returned after. A
// lease listed but not released was renewed in between, and is not an
// event.
func (r *recorder) expire(ctx context.Context, before []store.Lease, run func() ([]store.Lease, error)) ([]store.Lease, error) {
	after, err := run()
	if err != nil {
		return nil, fmt.Errorf("expire leases: %w", err)
	}
	prior := make(map[string]store.Lease, len(before))
	for _, l := range before {
		prior[l.ID] = l
	}
	for _, l := range after {
		project, err := r.tx.GetNode(ctx, store.GetNodeParams{OrgID: r.orgID, ID: l.NodeID})
		if err != nil {
			return nil, fmt.Errorf("read node %s: %w", l.NodeID, err)
		}
		reason := ReleaseExpired
		if err := r.event(ctx, OpExpireLease, l.NodeID, project.ProjectID, &reason, prior[l.ID], l); err != nil {
			return nil, err
		}
	}
	return after, nil
}
