-- name: CreateNode :one
INSERT INTO nodes (id, number, org_id, project_id, parent_id, kind, title, body, priority, assignee_kind, assignee, labels)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetNode :one
SELECT * FROM nodes
WHERE org_id = $1 AND id = $2;

-- name: GetNodeByNumber :one
SELECT * FROM nodes
WHERE org_id = $1 AND project_id = $2 AND number = $3;

-- name: GetNodesForUpdate :many
SELECT * FROM nodes
WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::text[])
ORDER BY id
FOR UPDATE;

-- name: ListNodes :many
SELECT * FROM nodes
WHERE org_id = $1 AND project_id = $2
ORDER BY number
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: ListChildren :many
SELECT * FROM nodes
WHERE org_id = $1 AND parent_id = sqlc.arg(parent_id)::text
ORDER BY number;

-- name: UpdateNode :one
UPDATE nodes
SET parent_id = $3, kind = $4, title = $5, body = $6, priority = $7,
    assignee_kind = $8, assignee = $9, labels = $10
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: SetNodeStatus :one
UPDATE nodes
SET status = $3
WHERE org_id = $1 AND id = $2
RETURNING *;
