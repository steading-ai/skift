-- name: CreateEvent :one
INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, on_behalf_of, identity_scheme, request_id, op, reason, before, after, policy_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- The stream: everything in an organization after a cursor, oldest first.
-- name: ListEventsAfter :many
SELECT * FROM events
WHERE org_id = $1 AND id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(page_size);

-- Context recovery: the history of one node, in order.
-- name: ListEventsForNode :many
SELECT * FROM events
WHERE org_id = $1 AND node_id = $2
ORDER BY id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- One patch, reconstructed: every event carrying its request id, in the
-- order the operations were applied.
-- name: ListEventsForRequest :many
SELECT * FROM events
WHERE org_id = $1 AND request_id = $2
ORDER BY id;

-- Whether a user principal has ever written to a node. Derived from the
-- log, never stored: see the Event section of docs/domain-model.md.
-- name: NodeHumanTouched :one
SELECT EXISTS (
    SELECT 1 FROM events
    WHERE org_id = $1 AND node_id = $2 AND principal_kind = 'user'
)::boolean AS human_touched;

-- The change stream: events of an organization from a transaction
-- boundary, in transaction and then id order, leaving out the ids a
-- consumer has already been handed. A project and a set of operations
-- narrow it; an empty set of operations is every operation. Events before
-- the replay window are not offered, so the partitions holding them are
-- pruned from the scan.
-- name: ListEventsFromPosition :many
SELECT * FROM events
WHERE org_id = $1
  AND tx_id >= sqlc.arg(min_tx_id)
  AND occurred_at >= sqlc.arg(retained_since)
  AND NOT (id = ANY(sqlc.arg(seen)::text[]))
  AND (sqlc.narg(project_id)::text IS NULL OR project_id = sqlc.narg(project_id))
  AND (cardinality(sqlc.arg(ops)::text[]) = 0 OR op = ANY(sqlc.arg(ops)::text[]))
ORDER BY tx_id, id
LIMIT sqlc.arg(page_size);

-- The positions of every event of an organization from a transaction
-- boundary: what a stream opened at "now" has to count as already seen.
-- name: ListEventPositionsFrom :many
SELECT tx_id, id FROM events
WHERE org_id = $1
  AND tx_id >= sqlc.arg(min_tx_id)
  AND occurred_at >= sqlc.arg(retained_since)
ORDER BY tx_id, id;
