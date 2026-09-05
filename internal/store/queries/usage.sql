-- name: CreateUsage :one
INSERT INTO usage (id, org_id, node_id, lease_id, principal, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, kind, basis)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: ListUsageForNode :many
SELECT * FROM usage
WHERE org_id = $1 AND node_id = $2 AND kind = $3
ORDER BY created_at DESC, id DESC;

-- name: ListAllUsageForNode :many
SELECT * FROM usage
WHERE org_id = $1 AND node_id = $2
ORDER BY created_at, id;

-- The current estimate is the most recent one: derived, never stored on
-- the node.
-- name: GetCurrentEstimate :one
SELECT * FROM usage
WHERE org_id = $1 AND node_id = $2 AND kind = 'estimate'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- Actuals over a node and every descendant through the parent relation,
-- grouped by model. Computed on each read; nothing stores a total. UNION
-- rather than UNION ALL so the walk ends even if a patch in flight has made
-- the parent relation cyclic before the cycle check has run over it.
-- name: RollupActuals :many
WITH RECURSIVE subtree AS (
    SELECT n.id FROM nodes n WHERE n.org_id = $1 AND n.id = $2
    UNION
    SELECT c.id FROM subtree s JOIN nodes c ON c.org_id = $1 AND c.parent_id = s.id
)
SELECT u.model,
    count(*)::bigint                  AS records,
    sum(u.input_tokens)::bigint       AS input_tokens,
    sum(u.output_tokens)::bigint      AS output_tokens,
    sum(u.cache_read_tokens)::bigint  AS cache_read_tokens,
    sum(u.cache_write_tokens)::bigint AS cache_write_tokens
FROM usage u JOIN subtree s ON s.id = u.node_id
WHERE u.org_id = $1 AND u.kind = 'actual'
GROUP BY u.model
ORDER BY u.model;

-- The current estimate of a node and every descendant, grouped by model.
-- Each node contributes its most recent estimate alone; superseded ones
-- stay in the table as history and count for nothing here.
-- name: RollupEstimates :many
WITH RECURSIVE subtree AS (
    SELECT n.id FROM nodes n WHERE n.org_id = $1 AND n.id = $2
    UNION
    SELECT c.id FROM subtree s JOIN nodes c ON c.org_id = $1 AND c.parent_id = s.id
),
current AS (
    SELECT DISTINCT ON (u.node_id) u.*
    FROM usage u JOIN subtree s ON s.id = u.node_id
    WHERE u.org_id = $1 AND u.kind = 'estimate'
    ORDER BY u.node_id, u.created_at DESC, u.id DESC
)
SELECT c.model,
    count(*)::bigint                  AS records,
    sum(c.input_tokens)::bigint       AS input_tokens,
    sum(c.output_tokens)::bigint      AS output_tokens,
    sum(c.cache_read_tokens)::bigint  AS cache_read_tokens,
    sum(c.cache_write_tokens)::bigint AS cache_write_tokens
FROM current c
GROUP BY c.model
ORDER BY c.model;
