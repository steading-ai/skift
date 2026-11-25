package domain

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steading-ai/skift/internal/store"
)

// ReadyHoldBlocker is one hold affecting matching open work. Downstream
// includes parked descendants as well as blocked dependents, but never
// the held ticket itself.
type ReadyHoldBlocker struct {
	Reference            string
	Reason               string
	DownstreamCount      int64
	DownstreamReferences []string
}

const (
	readyHoldLimit       = 5
	readyDownstreamLimit = 3
)

// Walk from stored holds rather than from every open ticket. Containment
// first carries each node hold down to the open nodes it parks, even if
// a node between them is closed. Then walk the live wait relation in
// reverse: open nodes block open dependents, and open children block
// open parents. A dropped gate still blocks, but releasing a hold on it
// would not change that. UNION expands each hold/node pair only once.
const readyHoldReach = `
WITH RECURSIVE scope AS (
    SELECT id, slug, held_at, held_reason
    FROM projects
    WHERE org_id = $1 AND archived_at IS NULL
      AND (COALESCE(cardinality($2::text[]), 0) = 0 OR id = ANY($2::text[]))
), node_holds AS (
    SELECT n.id, s.slug || '#' || n.number AS ref, n.held_reason AS reason
    FROM nodes n JOIN scope s ON s.id = n.project_id
    WHERE n.org_id = $1 AND n.held_at IS NOT NULL
), inherited(hold_id, node_id, status) AS (
    SELECT h.id, n.id, n.status
    FROM node_holds h JOIN nodes n ON n.org_id = $1 AND n.id = h.id
  UNION
    SELECT i.hold_id, c.id, c.status
    FROM inherited i JOIN nodes c ON c.org_id = $1 AND c.parent_id = i.node_id
), holds AS (
    SELECT id, false AS project_hold, ref, reason FROM node_holds
    UNION ALL
    SELECT id, true, slug, held_reason FROM scope WHERE held_at IS NOT NULL
), seeds(hold_id, project_hold, node_id) AS (
    SELECT hold_id, false, node_id FROM inherited WHERE status = 'open'
    UNION ALL
    SELECT s.id, true, n.id
    FROM scope s JOIN nodes n ON n.org_id = $1 AND n.project_id = s.id
    WHERE s.held_at IS NOT NULL AND n.status = 'open'
), reach(hold_id, project_hold, node_id) AS (
    SELECT hold_id, project_hold, node_id FROM seeds
  UNION
    SELECT r.hold_id, r.project_hold, n.id
    FROM reach r
    CROSS JOIN LATERAL (
        SELECT e.to_node AS node_id
        FROM edges e
        WHERE e.org_id = $1 AND e.from_node = r.node_id AND e.kind = 'blocks'
      UNION ALL
        SELECT c.parent_id
        FROM nodes c
        WHERE c.org_id = $1 AND c.id = r.node_id AND c.parent_id IS NOT NULL
    ) target
    JOIN nodes n ON n.org_id = $1 AND n.id = target.node_id AND n.status = 'open'
    JOIN scope s ON s.id = n.project_id
)`

// readyHoldBlockers ranks hold summaries by distinct downstream impact.
// The match filters select affected work, not sources or intermediate
// blockers: for example, a held user gate can prevent agent work.
func readyHoldBlockers(ctx context.Context, tx store.Tx, orgID string, q ReadyQuery, parentID string) ([]ReadyHoldBlocker, int64, error) {
	where, args, err := q.matchClauses("TRUE", parentID, []any{orgID, q.Projects})
	if err != nil {
		return nil, 0, err
	}
	args = append(args, readyDownstreamLimit, readyHoldLimit)
	sql := readyHoldReach + fmt.Sprintf(`
, matched AS (
    SELECT r.hold_id, r.project_hold, s.slug, n.number,
           (r.project_hold OR n.id <> r.hold_id) AS downstream
    FROM reach r
    JOIN nodes n ON n.org_id = $1 AND n.id = r.node_id
    JOIN scope s ON s.id = n.project_id
    WHERE %s
), ranked AS (
    SELECT *, row_number() OVER (PARTITION BY hold_id, project_hold, downstream ORDER BY slug, number) AS rank
    FROM matched
), summary AS (
    SELECT hold_id, project_hold, count(*) FILTER (WHERE downstream) AS downstream_count,
           COALESCE(array_agg(slug || '#' || number ORDER BY slug, number)
                    FILTER (WHERE downstream AND rank <= $%d), ARRAY[]::text[]) AS refs
    FROM ranked
    GROUP BY hold_id, project_hold
)
SELECT h.ref, h.reason, s.downstream_count, s.refs, count(*) OVER ()
FROM summary s JOIN holds h ON h.id = s.hold_id AND h.project_hold = s.project_hold
ORDER BY s.downstream_count DESC, h.ref
LIMIT $%d`, where, len(args)-1, len(args))
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query ready hold blockers: %w", err)
	}
	var total int64
	blockers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReadyHoldBlocker, error) {
		var h ReadyHoldBlocker
		err := row.Scan(&h.Reference, &h.Reason, &h.DownstreamCount, &h.DownstreamReferences, &total)
		return h, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("read ready hold blockers: %w", err)
	}
	return blockers, total, nil
}
