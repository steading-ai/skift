package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

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
// rather than UNION ALL for the reason FindWaitCycle's walk uses it: re-expanding a
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

// heldWhere is the effectively-held predicate on n, with $1 the
// organization: the same two conditions Ready negates, un-negated. It is
// composed from those fragments rather than written again beside them, so
// a condition cannot be added to one and forgotten in the other. The flat
// project probe comes first, so a held project answers for every one of
// its nodes without instantiating the walk.
const heldWhere = `(EXISTS (` + heldProject + `) OR EXISTS (` + heldAncestors + `))`

// derivedSQL computes every flag of a set of nodes in one statement, from
// the readiness fragments Ready and Explain share. $1 is the organization,
// $2 the node ids.
//
// Readiness itself is the ready query's own predicate rather than a
// conjunction restated in Go, so the flag on a node and its membership of
// the ready set cannot disagree. The per-condition flags stay because
// they say which condition holds, which the predicate alone does not.
const derivedSQL = `
SELECT n.id, n.number, pr.slug, n.status,
       (` + readyWhere + `) AS ready,
       EXISTS (` + unsatisfiedBlockers + `) AS blocked,
       EXISTS (` + liveLease + `) AS claimed,
       EXISTS (` + openChildren + `) AS open_child,
       EXISTS (` + archivedProject + `) AS archived,
       EXISTS (` + heldAncestors + `) AS held,
       EXISTS (` + heldProject + `) AS project_held,
       EXISTS (SELECT 1 FROM events ev
               WHERE ev.org_id = $1 AND ev.node_id = n.id AND ev.principal_kind = 'user') AS human_touched
FROM nodes n
JOIN projects pr ON pr.org_id = $1 AND pr.id = n.project_id
WHERE n.org_id = $1 AND n.id = ANY($2::text[])`

// DerivedRow is one row of derivedSQL: a node's reference, its status, and
// every readiness condition as a flag.
type DerivedRow struct {
	ID           string `db:"id"`
	Number       int64  `db:"number"`
	Slug         string `db:"slug"`
	Status       string `db:"status"`
	Blocked      bool   `db:"blocked"`
	Claimed      bool   `db:"claimed"`
	OpenChild    bool   `db:"open_child"`
	Archived     bool   `db:"archived"`
	Held         bool   `db:"held"`
	ProjectHeld  bool   `db:"project_held"`
	Ready        bool   `db:"ready"`
	HumanTouched bool   `db:"human_touched"`
}

// DeriveNodes computes the derived flags of the nodes ids of organization
// orgID in one statement, one row per node found.
func (t Tx) DeriveNodes(ctx context.Context, orgID string, ids []string) ([]DerivedRow, error) {
	rows, err := t.tx.Query(ctx, derivedSQL, orgID, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DerivedRow])
}

// ReadyFilter narrows a set of nodes by the filters a caller asks for,
// apart from its projects. Its zero value narrows nothing.
type ReadyFilter struct {
	// Kinds keeps nodes of any of these kinds.
	Kinds []string
	// AssigneeKind is "", "agent" or "user". "agent" also keeps nodes with
	// no assignee kind, which are agent-eligible; "user" keeps gates.
	AssigneeKind string
	// Labels keeps nodes carrying every one of them, and AnyLabels nodes
	// carrying at least one.
	Labels    []string
	AnyLabels []string
	// MinPriority keeps nodes at this priority or more urgent, p0 through
	// p4.
	MinPriority string
	// ParentID keeps the children of one node.
	ParentID string
}

// clauses appends f's clauses to base, an existing predicate on nodes n
// whose parameters are args, and returns the predicate with its
// parameters. A ready query applies them to eligible nodes; the hold
// summary applies them to the affected work without filtering away its
// blockers.
func (f ReadyFilter) clauses(base string, args []any) (string, []any, error) {
	var b strings.Builder
	b.WriteString(base)
	// where appends a clause whose single %d is the position of value.
	where := func(clause string, value any) {
		args = append(args, value)
		b.WriteString(" AND ")
		fmt.Fprintf(&b, clause, len(args))
	}

	if len(f.Kinds) > 0 {
		where("n.kind = ANY($%d::text[])", f.Kinds)
	}
	switch f.AssigneeKind {
	case "":
	case "agent":
		b.WriteString(" AND (n.assignee_kind IS NULL OR n.assignee_kind = 'agent')")
	case "user":
		b.WriteString(" AND n.assignee_kind = 'user'")
	default:
		return "", nil, fmt.Errorf("%w: assignee kind %q is not agent or user", ErrInvalidFilter, f.AssigneeKind)
	}
	if len(f.Labels) > 0 {
		where("n.labels @> $%d::text[]", f.Labels)
	}
	if len(f.AnyLabels) > 0 {
		where("n.labels && $%d::text[]", f.AnyLabels)
	}
	if f.MinPriority != "" {
		// Priorities are 'p0'..'p4', so text order is urgency order.
		where("n.priority <= $%d", f.MinPriority)
	}
	if f.ParentID != "" {
		where("n.parent_id = $%d", f.ParentID)
	}
	return b.String(), args, nil
}

// ReadyPage orders and bounds a ready query. Ordered is the queue:
// priority, then age, then id. Unordered output is by id, only so that it
// is deterministic.
type ReadyPage struct {
	Ordered bool
	Limit   int
	Offset  int
}

// readySQL renders the ready query. $1 is always the organization, so a
// filter can narrow the set and never widen it.
//
// The rendered statement selects n's columns from `nodes n`, so a caller that
// needs to lock what it reads may append `FOR UPDATE OF n SKIP LOCKED`.
func readySQL(orgID string, projects []string, f ReadyFilter, page ReadyPage) (string, []any, error) {
	where, args, err := f.clauses(readyWhere, []any{orgID})
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString("SELECT " + nodeColumns + " FROM nodes n WHERE " + where)
	if len(projects) > 0 {
		args = append(args, projects)
		fmt.Fprintf(&b, " AND n.project_id = ANY($%d::text[])", len(args))
	}

	if page.Ordered {
		b.WriteString(" ORDER BY n.priority, n.created_at, n.id")
	} else {
		b.WriteString(" ORDER BY n.id")
	}
	args = append(args, page.Limit)
	fmt.Fprintf(&b, " LIMIT $%d", len(args))
	if page.Offset > 0 {
		args = append(args, page.Offset)
		fmt.Fprintf(&b, " OFFSET $%d", len(args))
	}
	return b.String(), args, nil
}

// ReadyNodes returns the ready nodes of organization orgID in projects
// (every project when empty) that f keeps, ordered and bounded by page.
// The outer scan of an ordered page over one project is
// nodes_open_by_project_idx, which is in queue order.
func (t Tx) ReadyNodes(ctx context.Context, orgID string, projects []string, f ReadyFilter, page ReadyPage) ([]Node, error) {
	sql, args, err := readySQL(orgID, projects, f, page)
	if err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query ready set: %w", err)
	}
	nodes, err := pgx.CollectRows(rows, pgx.RowToStructByName[Node])
	if err != nil {
		return nil, fmt.Errorf("scan ready set: %w", err)
	}
	return nodes, nil
}

// LockNextReady selects the first node of the ordered ready set that f
// keeps in projects, and locks its row for the rest of the transaction,
// skipping rows other transactions hold. It returns [ErrNoRows] when
// there is none.
func (t Tx) LockNextReady(ctx context.Context, orgID string, projects []string, f ReadyFilter, limit int) (Node, error) {
	sql, args, err := readySQL(orgID, projects, f, ReadyPage{Ordered: true, Limit: limit})
	if err != nil {
		return Node{}, err
	}
	rows, err := t.tx.Query(ctx, sql+" FOR UPDATE OF n SKIP LOCKED", args...)
	if err != nil {
		return Node{}, err
	}
	return pgx.CollectOneRow(rows, pgx.RowToStructByName[Node])
}

// EligibleProjects returns, in no particular order, the ids of the
// projects among projects (every project of the organization when empty)
// that hold at least one ready node f keeps. It never locks a row, so it
// never conflicts with `FOR UPDATE`.
func (t Tx) EligibleProjects(ctx context.Context, orgID string, projects []string, f ReadyFilter) ([]string, error) {
	where, args, err := f.clauses(readyWhere, []any{orgID})
	if err != nil {
		return nil, err
	}
	sql := "SELECT DISTINCT n.project_id FROM nodes n WHERE " + where
	if len(projects) > 0 {
		args = append(args, projects)
		sql += fmt.Sprintf(" AND n.project_id = ANY($%d::text[])", len(args))
	}
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("eligible projects: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("eligible projects: %w", err)
	}
	return ids, nil
}

// IsReady reports whether one node satisfies the readiness predicate as of
// a fresh statement.
func (t Tx) IsReady(ctx context.Context, orgID, nodeID string) (bool, error) {
	var ready bool
	err := t.tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM nodes n WHERE "+readyWhere+" AND n.id = $2)", orgID, nodeID).Scan(&ready)
	return ready, err
}

// HeldCount is how many open nodes of a project are held rather than
// ready.
func (t Tx) HeldCount(ctx context.Context, orgID, projectID string) (int, error) {
	const sql = `SELECT count(*) FROM nodes n
WHERE n.org_id = $1 AND n.project_id = $2 AND n.status = 'open' AND ` + heldWhere
	var count int
	err := t.tx.QueryRow(ctx, sql, orgID, projectID).Scan(&count)
	return count, err
}

// HeldAncestor is one row of heldAncestors: a node in the chain that
// carries a hold. Its fields match that fragment's select list.
type HeldAncestor struct {
	Number     int64     `db:"number"`
	Title      string    `db:"title"`
	HeldAt     time.Time `db:"held_at"`
	HeldBy     string    `db:"held_by"`
	HeldReason string    `db:"held_reason"`
	HoldGroup  *string   `db:"hold_group"`
	Self       bool      `db:"self"`
}

// The readiness conditions of one node: each fragment above run
// un-negated for node nodeID of organization orgID, in the order a caller
// explaining the node reports it.

// ArchivedProjectOf is the node's project when it is archived.
func (t Tx) ArchivedProjectOf(ctx context.Context, orgID, nodeID string) ([]Project, error) {
	return explainRows[Project](ctx, t, archivedProject, "", orgID, nodeID)
}

// HeldProjectOf is the node's project when it is held.
func (t Tx) HeldProjectOf(ctx context.Context, orgID, nodeID string) ([]Project, error) {
	return explainRows[Project](ctx, t, heldProject, "", orgID, nodeID)
}

// HeldAncestorsOf is every hold on the node and on the nodes containing
// it, the node's own first, then by number.
func (t Tx) HeldAncestorsOf(ctx context.Context, orgID, nodeID string) ([]HeldAncestor, error) {
	return explainRows[HeldAncestor](ctx, t, heldAncestors, "ORDER BY x.self DESC, x.number", orgID, nodeID)
}

// UnsatisfiedBlockersOf is the node's unsatisfied predecessors, by project
// then number.
func (t Tx) UnsatisfiedBlockersOf(ctx context.Context, orgID, nodeID string) ([]Node, error) {
	return explainRows[Node](ctx, t, unsatisfiedBlockers, "ORDER BY x.project_id, x.number", orgID, nodeID)
}

// LiveLeaseOf is the node's live lease, of which there is at most one.
func (t Tx) LiveLeaseOf(ctx context.Context, orgID, nodeID string) ([]Lease, error) {
	return explainRows[Lease](ctx, t, liveLease, "", orgID, nodeID)
}

// OpenChildrenOf is the node's open children, by number.
func (t Tx) OpenChildrenOf(ctx context.Context, orgID, nodeID string) ([]Node, error) {
	return explainRows[Node](ctx, t, openChildren, "ORDER BY x.number", orgID, nodeID)
}

// explainRows runs one readiness subquery for the node $2 of organization
// $1 and collects its rows as T.
func explainRows[T any](ctx context.Context, t Tx, sub, order, orgID, nodeID string) ([]T, error) {
	sql := "SELECT " + selectList[T]("x") + " FROM nodes n CROSS JOIN LATERAL (" + sub + ") x WHERE n.org_id = $1 AND n.id = $2 " + order
	rows, err := t.tx.Query(ctx, sql, orgID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("explain node: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if err != nil {
		return nil, fmt.Errorf("explain node: %w", err)
	}
	return out, nil
}
