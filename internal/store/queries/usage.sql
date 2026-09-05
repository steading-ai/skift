-- name: CreateUsage :one
INSERT INTO usage (id, org_id, node_id, lease_id, principal, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, kind, basis)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: ListUsageForNode :many
SELECT * FROM usage
WHERE org_id = $1 AND node_id = $2 AND kind = $3
ORDER BY created_at DESC, id DESC;
