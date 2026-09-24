package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steading-ai/skift/internal/store"
)

// Relation is one kind of wait between two nodes.
type Relation string

const (
	// RelationBlocks is a `blocks` edge: the target waits for the source.
	RelationBlocks Relation = "blocks"
	// RelationParent is containment: a parent waits for each of its
	// children, because it is not ready while a child is open.
	RelationParent Relation = "parent"
)

// NodeRef names a node the way prose does: its project slug and its number,
// rendered as `slug#7`. See the Keys section of docs/domain-model.md.
type NodeRef struct {
	Project string
	Number  int64
}

func (r NodeRef) String() string {
	return fmt.Sprintf("%s#%d", r.Project, r.Number)
}

// Hop is one wait in a cycle: From waits for To because of Via.
type Hop struct {
	From, To NodeRef
	Via      Relation
}

// CycleError reports a cycle in the wait relation. Path is closed: each hop's
// To is the next hop's From, and the last hop returns to the first.
type CycleError struct {
	Path []Hop
}

func (e *CycleError) Error() string {
	var b strings.Builder
	b.WriteString("cycle in the wait relation: ")
	for i, hop := range e.Path {
		if i == 0 {
			b.WriteString(hop.From.String())
		} else {
			b.WriteString(", which")
		}
		switch hop.Via {
		case RelationParent:
			b.WriteString(" is the parent of ")
		default:
			b.WriteString(" is blocked by ")
		}
		b.WriteString(hop.To.String())
	}
	return b.String()
}

// waitReach walks the wait relation forward from a set of seed nodes. The
// relation is the union the Acyclicity section of docs/domain-model.md
// describes: a node waits for every open-or-not source of a `blocks` edge
// into it, and a parent waits for every child. Status is ignored on
// purpose; a done node can be reopened, so the invariant is structural.
//
// Rows are (start, node, prev, via): node was reached from start, and the
// last hop was prev waiting for node because of via. UNION rather than
// UNION ALL makes the row set the visited set, so a node reached along two
// paths expands once per relation into it rather than once per path, and
// the walk is bounded by the relations reachable from the seeds instead of
// the number of paths through them. A row whose node is its own start with
// a non-null prev is a closed cycle. Re-expanding it reproduces the rows
// the seed produced, which the UNION discards, so the walk terminates.
//
// Both branches of the step are index lookups: edges_to_node_idx and
// nodes_parent_id_idx. The organization predicate is static SQL on every
// table touched.
const waitReach = `
WITH RECURSIVE reach(start, node, prev, via) AS (
    SELECT s.id, s.id, NULL::text, NULL::text
    FROM unnest($2::text[]) AS s(id)
  UNION
    SELECT r.start, w.node, r.node, w.via
    FROM reach r
    CROSS JOIN LATERAL (
        SELECT e.from_node AS node, 'blocks'::text AS via
        FROM edges e
        WHERE e.org_id = $1 AND e.to_node = r.node AND e.kind = 'blocks'
      UNION ALL
        SELECT c.id, 'parent'::text
        FROM nodes c
        WHERE c.org_id = $1 AND c.parent_id = r.node
    ) w
)`

// findWaitCycle returns every hop of the walk from any seed that closed a
// cycle, with the reference of the reached node. It returns no rows when
// the graph is acyclic, so the common case moves nothing but the verdict.
const findWaitCycle = waitReach + `
SELECT r.start, r.node, r.prev, r.via, p.slug, n.number
FROM reach r
JOIN nodes n ON n.org_id = $1 AND n.id = r.node
JOIN projects p ON p.org_id = $1 AND p.id = n.project_id
WHERE r.start IN (SELECT start FROM reach WHERE node = start AND prev IS NOT NULL)
ORDER BY r.start, r.node, r.prev`

// CheckAcyclic verifies that the wait relation, as tx now sees it, contains
// no cycle through any of the touched nodes. It runs once per patch, after
// every operation has been applied, so it judges the resulting graph rather
// than each operation in turn; a patch that removes an edge and adds its
// reverse is acyclic and passes.
//
// Any cycle a patch introduces contains a wait the patch added, so touched
// must include the waiting side of each: the target of every added edge,
// and the new parent of every created or reparented node. Extra ids only
// cost the walk their reachable set. Under the organization advisory lock
// the Concurrency section of docs/domain-model.md describes, a passing
// check holds at commit.
//
// A cycle is returned as a [*CycleError] naming its path in node
// references.
func CheckAcyclic(ctx context.Context, tx store.Tx, orgID string, touched []string) error {
	if len(touched) == 0 {
		return nil
	}
	rows, err := tx.Tx.Query(ctx, findWaitCycle, orgID, touched)
	if err != nil {
		return fmt.Errorf("walk wait relation: %w", err)
	}
	defer rows.Close()

	var hops []reachRow
	for rows.Next() {
		var r reachRow
		if err := rows.Scan(&r.start, &r.node, &r.prev, &r.via, &r.ref.Project, &r.ref.Number); err != nil {
			return fmt.Errorf("scan wait relation: %w", err)
		}
		hops = append(hops, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("walk wait relation: %w", err)
	}
	if len(hops) == 0 {
		return nil
	}
	return &CycleError{Path: closePath(hops)}
}

// reachRow is one row of findWaitCycle.
type reachRow struct {
	start, node string
	prev, via   *string
	ref         NodeRef
}

// closePath reconstructs one cycle from the rows of a walk that closed. It
// takes the first start in row order, walks backwards from the node that
// returned to it, and returns the shortest closed path. Rows may include
// hops of other cycles through other seeds, so the backward walk keeps a
// visited set.
func closePath(rows []reachRow) []Hop {
	start := rows[0].start
	refs := map[string]NodeRef{}
	preds := map[string][]reachRow{}
	var closing *reachRow
	for i := range rows {
		r := rows[i]
		if r.start != start {
			continue
		}
		refs[r.node] = r.ref
		if r.prev == nil {
			continue
		}
		preds[r.node] = append(preds[r.node], r)
		if r.node == start && closing == nil {
			closing = &rows[i]
		}
	}
	if closing == nil {
		// Unreachable by construction: every start returned has a closing row.
		panic(errors.New("domain: cycle rows without a closing hop"))
	}

	// Breadth-first from the node that closed the cycle back to the start,
	// remembering how each node was first reached.
	via := map[string]reachRow{*closing.prev: *closing}
	queue := []string{*closing.prev}
	for len(queue) > 0 && queue[0] != start {
		cur := queue[0]
		queue = queue[1:]
		for _, p := range preds[cur] {
			if _, seen := via[*p.prev]; seen {
				continue
			}
			via[*p.prev] = p
			queue = append(queue, *p.prev)
		}
	}

	// Unwind from start through the recorded hops until the cycle closes.
	var path []Hop
	for at := start; ; {
		hop, ok := via[at]
		if !ok {
			// Unreachable by construction: every row was reached from start.
			panic(errors.New("domain: cycle rows do not lead back to their start"))
		}
		path = append(path, Hop{From: refs[*hop.prev], To: refs[hop.node], Via: Relation(*hop.via)})
		at = hop.node
		if at == start {
			return path
		}
	}
}
