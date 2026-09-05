-- name: CreatePolicyDenial :one
INSERT INTO policy_denials (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, request_id, op, reasons, policy_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- The audit of one organization's refusals after a cursor, oldest first.
-- name: ListPolicyDenialsAfter :many
SELECT * FROM policy_denials
WHERE org_id = $1 AND id > sqlc.arg(after_id)
ORDER BY id
LIMIT sqlc.arg(page_size);
