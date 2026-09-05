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
