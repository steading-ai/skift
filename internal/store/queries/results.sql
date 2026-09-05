-- name: CreateResult :one
INSERT INTO results (id, org_id, node_id, body, completed_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- A node's results, newest first: the current one is the first row.
-- name: ListResultsForNode :many
SELECT * FROM results
WHERE org_id = $1 AND node_id = $2
ORDER BY created_at DESC, id DESC;
