-- name: CreateResult :one
INSERT INTO results (id, org_id, node_id, body, completed_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- A node's results, newest first: the current one is the first row.
-- name: ListResultsForNode :many
SELECT * FROM results
WHERE org_id = $1 AND node_id = $2
ORDER BY created_at DESC, id DESC;

-- InputsSize counts the current results of the blocking predecessors of
-- node $2 of organization $1 and adds up their bodies. Only a done
-- predecessor has a current result, which is the newest of its rows.
-- name: InputsSize :one
SELECT count(*)::int AS count, coalesce(sum(octet_length(r.body)), 0)::bigint AS bytes
FROM edges e
JOIN nodes b ON b.org_id = e.org_id AND b.id = e.from_node
JOIN LATERAL (
    SELECT body FROM results
    WHERE org_id = b.org_id AND node_id = b.id
    ORDER BY created_at DESC, id DESC
    LIMIT 1
) r ON true
WHERE e.org_id = $1 AND e.to_node = $2 AND e.kind = 'blocks' AND b.status = 'done';
