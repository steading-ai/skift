package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ivy/dagger/internal/store"
)

// ErrOneProject is returned by [Ready] when the query does not name exactly
// one project. Priority is comparable only within a project, so an ordered
// ready set across several would be arithmetic on incomparable scales.
var ErrOneProject = errors.New("an ordered ready query names exactly one project")

// ErrNodeNotFound is returned by [Explain] when the organization has no such
// node.
var ErrNodeNotFound = errors.New("node not found")

// Assignee kinds, matching Principal.kind. See the Node section of
// docs/domain-model.md.
const (
	// AssigneeAgent marks a node as agent work. A node with no assignee
	// kind is agent-eligible too, so a filter on this kind includes it.
	AssigneeAgent = "agent"
	// AssigneeUser makes a node a gate: only completion by a user satisfies
	// the edges out of it.
	AssigneeUser = "user"
)

// ReadyQuery is the caller's filters over the ready set. The zero value
// matches every ready node in the organization.
type ReadyQuery struct {
	// Projects restricts the set to these project ids. Empty means every
	// project. An ordered query names exactly one.
	Projects []string
	// Kind restricts the set to one node kind. Empty means every kind.
	Kind string
	// AssigneeKind restricts the set to nodes completable by that kind of
	// principal: [AssigneeUser] selects gates; [AssigneeAgent] selects
	// agent-assigned nodes and nodes with no assignee kind, which are
	// agent-eligible. Empty means any.
	AssigneeKind string
	// Labels restricts the set to nodes carrying every one of them.
	Labels []string
	// MinPriority restricts the set to nodes at this priority or more
	// urgent, `p0` through `p4`. Empty means any.
	MinPriority string
	// Limit caps the number of rows returned, at most [Limits.PageSize].
	// Zero takes that ceiling.
	Limit int
	// Offset skips that many rows of the ordered set, for the next page.
	// Only an ordered query pages.
	Offset int
}

// Each readiness condition is one subquery over the node aliased n, and
// each is written exactly once. Ready negates it inside NOT EXISTS; Explain
// selects from it un-negated. The organization predicate is static SQL on
// every table touched.

// unsatisfiedBlockers are the sources of `blocks` edges into n that have
// not satisfied them: open nodes, and gates that were not completed. A
// dropped gate still blocks, so an agent cannot drop its own gate and
// proceed. See the Gates section of docs/domain-model.md.
//
// edges_to_node_idx serves the lookup.
const unsatisfiedBlockers = `
SELECT b.*
FROM edges e
JOIN nodes b ON b.org_id = e.org_id AND b.id = e.from_node
WHERE e.org_id = $1 AND e.to_node = n.id AND e.kind = 'blocks'
  AND (b.status = 'open' OR (b.assignee_kind = 'user' AND b.status <> 'done'))`

// liveLease is the unreleased lease on n whose TTL has not passed. There is
// at most one: leases_live_idx.
const liveLease = `
SELECT l.*
FROM leases l
WHERE l.org_id = $1 AND l.node_id = n.id AND l.released_at IS NULL AND l.expires_at > now()`

// openChildren are the open nodes whose parent is n. A node that groups
// work is not itself work until that work closes. nodes_open_children_idx
// serves the lookup.
const openChildren = `
SELECT c.*
FROM nodes c
WHERE c.org_id = $1 AND c.parent_id = n.id AND c.status = 'open'`

// archivedProject is n's project when it is archived, and so read-only: no
// claims. See the Project section of docs/domain-model.md.
const archivedProject = `
SELECT p.*
FROM projects p
WHERE p.org_id = $1 AND p.id = n.project_id AND p.archived_at IS NOT NULL`

// heldAncestors are n itself and the nodes containing it, up through
// parent_id, that carry a hold: a stored decision to park work. A hold
// takes a node out of the ready set without changing its status, the way
// a live lease does, and containment carries it, because holding an epic
// that did not hold its children would hold nothing claimable. See the
// Hold section of docs/domain-model.md.
//
// The walk goes to the root. There is no depth ceiling on the parent
// chain, and a bounded walk would return a node held from further up to
// the ready set, which is the failure the hold exists to prevent. UNION
// rather than UNION ALL for the reason waitReach uses it: re-expanding a
// node reproduces a row already emitted, so the walk terminates rather
// than spinning to the statement timeout. The step is the (project_id,
// id) lookup nodes_project_id_id_key serves, and nodes_parent_fkey pins a
// parent to its child's project, so the walk cannot leave the project the
// seed scoped it to.
//
// The chain carries only what the walk needs, because UNION de-duplicates
// on every column it selects; the hold itself is joined back once at the
// end, where Explain scans these rows into a struct by name. EXISTS
// ignores that final list, so the other call sites pay nothing for it.
const heldAncestors = `
WITH RECURSIVE chain AS (
    SELECT a.id, a.parent_id, a.project_id
    FROM nodes a
    WHERE a.org_id = $1 AND a.id = n.id
  UNION
    SELECT p.id, p.parent_id, p.project_id
    FROM chain
    JOIN nodes p ON p.project_id = chain.project_id AND p.id = chain.parent_id
)
SELECT h.number, h.title, h.held_at, h.held_by, h.held_reason,
       h.hold_group, (h.id = n.id) AS self
FROM chain c
JOIN nodes h ON h.project_id = c.project_id AND h.id = c.id
WHERE h.held_at IS NOT NULL`

// heldProject is n's project when it is held: every node in it leaves the
// ready set and none of their statuses change. Unlike an archived
// project, a held one is still writable. projects_held_idx serves the
// lookup.
const heldProject = `
SELECT p.*
FROM projects p
WHERE p.org_id = $1 AND p.id = n.project_id AND p.held_at IS NOT NULL`

// readyWhere is the readiness predicate on n, with $1 the organization.
const readyWhere = `n.org_id = $1 AND n.status = 'open'
  AND NOT EXISTS (` + unsatisfiedBlockers + `)
  AND NOT EXISTS (` + liveLease + `)
  AND NOT EXISTS (` + openChildren + `)
  AND NOT EXISTS (` + archivedProject + `)
  AND NOT EXISTS (` + heldProject + `)
  AND NOT EXISTS (` + heldAncestors + `)`

// Ready returns the ready nodes of one project that match q, most urgent
// and then oldest first: priority, created_at, id. The outer scan is
// nodes_open_by_project_idx, which is in that order. q must name exactly
// one project, or [ErrOneProject] is returned.
func Ready(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery) ([]store.Node, error) {
	if len(q.Projects) != 1 {
		return nil, ErrOneProject
	}
	if q.Offset < 0 {
		return nil, errors.New("offset is negative")
	}
	return queryReady(ctx, tx, orgID, q, true)
}

// ReadySet returns the ready nodes matching q across any number of
// projects, in no meaningful order. It exists for callers that want the
// set rather than a queue: reporting, and the from-scratch recomputation a
// property test compares against.
func ReadySet(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery) ([]store.Node, error) {
	return queryReady(ctx, tx, orgID, q, false)
}

func queryReady(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery, ordered bool) ([]store.Node, error) {
	sql, args, err := q.sql(orgID, ordered)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query ready set: %w", err)
	}
	nodes, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Node])
	if err != nil {
		return nil, fmt.Errorf("scan ready set: %w", err)
	}
	return nodes, nil
}

// sql renders the ready query. $1 is always the organization, so a filter
// can narrow the set and never widen it. Ordered output is by priority
// then age; unordered output is by id, only so that it is deterministic.
//
// The rendered statement selects `n.*` from `nodes n`, so a caller that
// needs to lock what it reads may append `FOR UPDATE OF n SKIP LOCKED`.
func (q ReadyQuery) sql(orgID string, ordered bool) (string, []any, error) {
	var b strings.Builder
	b.WriteString("SELECT n.* FROM nodes n WHERE " + readyWhere)
	args := []any{orgID}
	// where appends a clause whose single %d is the position of value.
	where := func(clause string, value any) {
		args = append(args, value)
		b.WriteString(" AND ")
		fmt.Fprintf(&b, clause, len(args))
	}

	if len(q.Projects) > 0 {
		where("n.project_id = ANY($%d::text[])", q.Projects)
	}
	if q.Kind != "" {
		where("n.kind = $%d", q.Kind)
	}
	switch q.AssigneeKind {
	case "":
	case AssigneeAgent:
		b.WriteString(" AND (n.assignee_kind IS NULL OR n.assignee_kind = 'agent')")
	case AssigneeUser:
		b.WriteString(" AND n.assignee_kind = 'user'")
	default:
		return "", nil, fmt.Errorf("assignee kind %q is not agent or user", q.AssigneeKind)
	}
	if len(q.Labels) > 0 {
		where("n.labels @> $%d::text[]", q.Labels)
	}
	if q.MinPriority != "" {
		if !validPriority(q.MinPriority) {
			return "", nil, fmt.Errorf("priority %q is not p0 through p4", q.MinPriority)
		}
		// Priorities are 'p0'..'p4', so text order is urgency order.
		where("n.priority <= $%d", q.MinPriority)
	}

	if ordered {
		b.WriteString(" ORDER BY n.priority, n.created_at, n.id")
	} else {
		b.WriteString(" ORDER BY n.id")
	}
	limit, err := boundPageSize(q.Limit, ActiveLimits())
	if err != nil {
		return "", nil, err
	}
	args = append(args, limit)
	fmt.Fprintf(&b, " LIMIT $%d", len(args))
	if q.Offset > 0 {
		args = append(args, q.Offset)
		fmt.Fprintf(&b, " OFFSET $%d", len(args))
	}
	return b.String(), args, nil
}

func validPriority(p string) bool {
	switch p {
	case "p0", "p1", "p2", "p3", "p4":
		return true
	}
	return false
}

// ReasonKind is one condition of readiness that does not hold.
type ReasonKind string

const (
	// ReasonNotOpen: the node is done or dropped.
	ReasonNotOpen ReasonKind = "not_open"
	// ReasonArchived: the node's project is archived and read-only.
	ReasonArchived ReasonKind = "archived"
	// ReasonBlocked: a predecessor has not satisfied its `blocks` edge.
	ReasonBlocked ReasonKind = "blocked"
	// ReasonLeased: a live lease holds the node.
	ReasonLeased ReasonKind = "leased"
	// ReasonOpenChild: an open child keeps the node from being ready.
	ReasonOpenChild ReasonKind = "open_child"
	// ReasonHeld: the node, or a node containing it, carries a hold.
	ReasonHeld ReasonKind = "held"
	// ReasonProjectHeld: the node's project carries a hold.
	ReasonProjectHeld ReasonKind = "project_held"
)

// Hold is the stored decision that keeps work out of the ready set: which
// node carries it, who applied it, when, and why. A hold is never
// permanent, so a held node is parked rather than stuck.
type Hold struct {
	// Number and Title identify the node carrying the hold.
	Number int64
	Title  string
	// Self reports that the hold is on the node being explained rather
	// than on something containing it.
	Self bool
	At   time.Time
	By   string
	// Reason is why the work was parked. Always set: a hold nobody can
	// explain is one nobody dares release.
	Reason string
	// Group is the focus sweep that placed the hold, if a sweep did.
	Group *string
}

// Reason is one structured answer to why a node is not ready. Kind says
// which of the optional fields is set.
type Reason struct {
	Kind ReasonKind
	// Status is the node's stored status, for [ReasonNotOpen].
	Status string
	// Blocker is the predecessor, for [ReasonBlocked].
	Blocker *store.Node
	// Gate reports that Blocker is user-assigned, so only its completion
	// by a user satisfies the edge: the node is waiting on a person.
	Gate bool
	// Lease is the live lease, with its holder and expiry, for
	// [ReasonLeased].
	Lease *store.Lease
	// Child is the open child, for [ReasonOpenChild].
	Child *store.Node
	// Project is the project, for [ReasonArchived] and
	// [ReasonProjectHeld].
	Project *store.Project
	// Hold is the hold keeping the node parked, for [ReasonHeld].
	Hold *Hold
	// Permanent reports that no transition of the graph can clear this
	// reason: a dropped gate, since dropped is terminal and a gate is
	// satisfied only by completion; or the node itself being dropped.
	Permanent bool
}

func (r Reason) String() string {
	switch r.Kind {
	case ReasonNotOpen:
		return "it is " + r.Status
	case ReasonArchived:
		return "its project " + r.Project.Slug + " is archived"
	case ReasonBlocked:
		what := "open node"
		if r.Gate {
			what = r.Blocker.Status + " gate"
		}
		return fmt.Sprintf("it is blocked by %s #%d", what, r.Blocker.Number)
	case ReasonLeased:
		return fmt.Sprintf("it is held by lease %s until %s", r.Lease.ID, r.Lease.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"))
	case ReasonOpenChild:
		return fmt.Sprintf("its child #%d is open", r.Child.Number)
	case ReasonHeld:
		where := fmt.Sprintf("it is contained in #%d, which is", r.Hold.Number)
		if r.Hold.Self {
			where = "it is"
		}
		return fmt.Sprintf("%s held since %s: %s", where,
			r.Hold.At.UTC().Format("2006-01-02T15:04:05Z"), r.Hold.Reason)
	case ReasonProjectHeld:
		return fmt.Sprintf("its project %s is held: %s", r.Project.Slug, *r.Project.HeldReason)
	}
	return string(r.Kind)
}

// Explanation is why one node is not ready. Reasons is empty when it is.
type Explanation struct {
	Node    store.Node
	Reasons []Reason
}

// Ready reports whether every readiness condition holds.
func (e *Explanation) Ready() bool {
	return len(e.Reasons) == 0
}

// Stuck reports a node with no possible path to readiness: it is open, and
// something nothing can clear keeps it out of the ready set. That is always
// a bug rather than a state, which is why it is worth detecting on its own.
// See the "Why not ready" section of docs/domain-model.md.
func (e *Explanation) Stuck() bool {
	if e.Node.Status != "open" {
		return false
	}
	for _, r := range e.Reasons {
		if r.Permanent {
			return true
		}
	}
	return false
}

// Explain answers why the node is not ready, as the same conditions Ready
// checks, un-negated. Reasons come in a fixed order: not open, archived,
// the project's hold, the holds on the node and what contains it, then
// blockers by project then number, the live lease, open children by
// number. Project-scoped facts come before node-scoped ones.
func Explain(ctx context.Context, tx store.Tx, orgID, nodeID string) (*Explanation, error) {
	node, err := tx.GetNode(ctx, store.GetNodeParams{OrgID: orgID, ID: nodeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("explain node: %w", err)
	}
	e := &Explanation{Node: node}

	if node.Status != "open" {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonNotOpen, Status: node.Status, Permanent: node.Status == "dropped"})
	}

	projects, err := explainRows[store.Project](ctx, tx, archivedProject, "", orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonArchived, Project: &projects[i]})
	}

	heldProjects, err := explainRows[store.Project](ctx, tx, heldProject, "", orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range heldProjects {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonProjectHeld, Project: &heldProjects[i]})
	}

	// Every hold in the chain, not only the nearest: the node becomes
	// ready when all of them are released, so reporting one would leave
	// an item off the list.
	holds, err := explainRows[heldRow](ctx, tx, heldAncestors, "ORDER BY x.self DESC, x.number", orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for _, h := range holds {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonHeld, Hold: &Hold{
			Number: h.Number,
			Title:  h.Title,
			Self:   h.Self,
			At:     h.HeldAt,
			By:     h.HeldBy,
			Reason: h.HeldReason,
			Group:  h.HoldGroup,
		}})
	}

	blockers, err := explainRows[store.Node](ctx, tx, unsatisfiedBlockers, "ORDER BY x.project_id, x.number", orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range blockers {
		b := &blockers[i]
		gate := b.AssigneeKind != nil && *b.AssigneeKind == AssigneeUser
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonBlocked, Blocker: b, Gate: gate, Permanent: gate && b.Status == "dropped"})
	}

	leases, err := explainRows[store.Lease](ctx, tx, liveLease, "", orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range leases {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonLeased, Lease: &leases[i]})
	}

	children, err := explainRows[store.Node](ctx, tx, openChildren, "ORDER BY x.number", orgID, nodeID)
	if err != nil {
		return nil, err
	}
	for i := range children {
		e.Reasons = append(e.Reasons, Reason{Kind: ReasonOpenChild, Child: &children[i]})
	}
	return e, nil
}

// heldRow is one row of heldAncestors: a node in the chain that carries a
// hold. Its fields match that fragment's select list.
type heldRow struct {
	Number     int64     `db:"number"`
	Title      string    `db:"title"`
	HeldAt     time.Time `db:"held_at"`
	HeldBy     string    `db:"held_by"`
	HeldReason string    `db:"held_reason"`
	HoldGroup  *string   `db:"hold_group"`
	Self       bool      `db:"self"`
}

// explainRows runs one readiness subquery for the node $2 of organization
// $1 and collects its rows as T.
func explainRows[T any](ctx context.Context, tx store.Tx, sub, order, orgID, nodeID string) ([]T, error) {
	sql := "SELECT x.* FROM nodes n CROSS JOIN LATERAL (" + sub + ") x WHERE n.org_id = $1 AND n.id = $2 " + order
	rows, err := tx.Tx.Query(ctx, sql, orgID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("explain node: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if err != nil {
		return nil, fmt.Errorf("explain node: %w", err)
	}
	return out, nil
}
