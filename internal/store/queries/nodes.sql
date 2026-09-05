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

-- A node and each of its ancestors up to the root, nearest first, with the
-- principal that created each: the actor of its create_node event, or empty
-- for a node with none, such as one whose event is about to be written.
-- This is what policy judges ownership from. The path guard stops the walk
-- if a patch in flight has made the parent relation cyclic before the cycle
-- check has run over it.
-- name: NodeLineage :many
WITH RECURSIVE chain AS (
    SELECT n.id, n.parent_id, n.project_id, ARRAY[n.id]::text[] AS path
    FROM nodes n
    WHERE n.org_id = $1 AND n.id = $2
    UNION ALL
    SELECT p.id, p.parent_id, p.project_id, c.path || p.id
    FROM chain c
    JOIN nodes p ON p.org_id = $1 AND p.id = c.parent_id
    WHERE NOT p.id = ANY(c.path)
)
SELECT c.id, c.parent_id, c.project_id,
    COALESCE((SELECT e.principal_id FROM events e
              WHERE e.org_id = $1 AND e.node_id = c.id AND e.op = 'create_node'
              ORDER BY e.id LIMIT 1), '')::text AS created_by
FROM chain c
ORDER BY array_length(c.path, 1);
