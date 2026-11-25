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

-- ListBlockingPredecessors is the sources of blocks edges into node $2 of
-- organization $1, whatever their status.
-- name: ListBlockingPredecessors :many
SELECT b.*
FROM edges e
JOIN nodes b ON b.org_id = e.org_id AND b.id = e.from_node
WHERE e.org_id = $1 AND e.to_node = $2 AND e.kind = 'blocks'
ORDER BY b.project_id, b.number;

-- FindWaitCycle walks the wait relation forward from the touched nodes
-- and returns every hop of the walk from any seed that closed a cycle,
-- with the reference of the reached node. It returns no rows when the
-- graph is acyclic, so the common case moves nothing but the verdict.
--
-- The relation is the union the Acyclicity section of docs/domain-model.md
-- describes: a node waits for every open-or-not source of a `blocks` edge
-- into it, and a parent waits for every child. Status is ignored on
-- purpose; a done node can be reopened, so the invariant is structural.
--
-- reach's rows are (start, node, prev, via): node was reached from start,
-- and the last hop was prev waiting for node because of via. UNION rather
-- than UNION ALL makes the row set the visited set, so a node reached along
-- two paths expands once per relation into it rather than once per path,
-- and the walk is bounded by the relations reachable from the seeds instead
-- of the number of paths through them. A row whose node is its own start
-- with a non-null prev is a closed cycle. Re-expanding it reproduces the
-- rows the seed produced, which the UNION discards, so the walk terminates.
--
-- Both branches of the step are index lookups: edges_to_node_idx and
-- nodes_parent_id_idx. The organization predicate is static SQL on every
-- table touched.
-- name: FindWaitCycle :many
WITH RECURSIVE reach(start, node, prev, via) AS (
    SELECT s.id::text, s.id::text, NULL::text, NULL::text
    FROM unnest(sqlc.arg(touched)::text[]) AS s(id)
  UNION
    SELECT r.start, w.node, r.node, w.via
    FROM reach r
    CROSS JOIN LATERAL (
        SELECT e.from_node AS node, 'blocks'::text AS via
        FROM edges e
        WHERE e.org_id = $1 AND e.to_node = r.node AND e.kind = 'blocks'
      UNION ALL
        SELECT c.id, 'parent'::text
        FROM nodes c
        WHERE c.org_id = $1 AND c.parent_id = r.node
    ) w
)
SELECT r.start, r.node, r.prev, r.via, p.slug, n.number
FROM reach r
JOIN nodes n ON n.org_id = $1 AND n.id = r.node
JOIN projects p ON p.org_id = $1 AND p.id = n.project_id
WHERE r.start IN (SELECT start FROM reach WHERE node = start AND prev IS NOT NULL)
ORDER BY r.start, r.node, r.prev;
