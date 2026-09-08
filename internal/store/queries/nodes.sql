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

-- Parks a node. Containment carries the hold at read time, so nothing is
-- written to its descendants. See the Hold section of docs/domain-model.md.
-- name: HoldNode :one
UPDATE nodes
SET held_at = now(), held_by = $3, held_reason = $4
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: UnholdNode :one
UPDATE nodes
SET held_at = NULL, held_by = NULL, held_reason = NULL, hold_group = NULL
WHERE org_id = $1 AND id = $2
RETURNING *;

-- Parks the nodes a sweep selected, skipping any that already carry a
-- hold: a node held on its own carries no hold_group and is left alone,
-- so clearing the sweep does not release it.
-- name: HoldNodeSet :many
UPDATE nodes
SET held_at = now(), held_by = $2, held_reason = $3, hold_group = $4
WHERE org_id = $1 AND held_at IS NULL AND id = ANY(sqlc.arg(ids)::text[])
RETURNING *;

-- The containment of every open node of a project, which is all a sweep
-- needs to choose what to park.
-- name: OpenNodesOfProject :many
SELECT id, parent_id FROM nodes
WHERE org_id = $1 AND project_id = $2 AND status = 'open';

-- Releases every node hold one sweep placed, and only those.
-- name: ReleaseNodeHoldGroup :many
UPDATE nodes
SET held_at = NULL, held_by = NULL, held_reason = NULL, hold_group = NULL
WHERE org_id = $1 AND hold_group = $2
RETURNING *;

-- The sweeps with live node holds in a project, so the active one can be
-- found without a caller remembering its id.
-- name: NodeHoldGroups :many
SELECT DISTINCT hold_group::text AS hold_group FROM nodes
WHERE org_id = $1 AND project_id = $2 AND hold_group IS NOT NULL
ORDER BY hold_group;
