-- name: CreateEdge :one
INSERT INTO edges (org_id, from_node, to_node, kind)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: DeleteEdge :execrows
DELETE FROM edges
WHERE org_id = $1 AND from_node = $2 AND to_node = $3 AND kind = $4;

-- Edges into a node: what blocks it.
-- name: ListEdgesInto :many
SELECT * FROM edges
WHERE org_id = $1 AND to_node = $2
ORDER BY from_node, kind;

-- Edges out of a node: what it blocks.
-- name: ListEdgesOutOf :many
SELECT * FROM edges
WHERE org_id = $1 AND from_node = $2
ORDER BY to_node, kind;

-- Every edge touching the given nodes, for cycle checks over a component.
-- name: ListEdgesAmong :many
SELECT * FROM edges
WHERE org_id = $1
  AND (from_node = ANY(sqlc.arg(ids)::text[]) OR to_node = ANY(sqlc.arg(ids)::text[]))
ORDER BY from_node, to_node, kind;
