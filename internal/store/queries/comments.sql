-- name: CreateComment :one
INSERT INTO comments (id, org_id, node_id, author, body, in_reply_to)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetComment :one
SELECT * FROM comments
WHERE org_id = $1 AND id = $2;

-- name: ListCommentsForNode :many
SELECT * FROM comments
WHERE org_id = $1 AND node_id = $2
ORDER BY created_at, id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- The prior body moves to comment_revisions by trigger.
-- name: UpdateCommentBody :one
UPDATE comments
SET body = $3
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: ResolveComment :one
UPDATE comments
SET resolved_at = now(), resolved_by = $3
WHERE org_id = $1 AND id = $2 AND resolved_at IS NULL
RETURNING *;

-- Revisions reach their organization through the comment they belong to.
-- name: ListCommentRevisions :many
SELECT r.* FROM comment_revisions AS r
JOIN comments AS c ON c.id = r.comment_id
WHERE c.org_id = $1 AND r.comment_id = $2
ORDER BY r.revision;

-- The newest unresolved comment on a node, with how many are unresolved:
-- the window counts before LIMIT applies, and a node with none returns no
-- row. A reply is its own row and is resolved on its own, so it counts
-- even under a resolved parent.
-- name: LatestUnresolvedComment :one
SELECT created_at, count(*) OVER ()::int AS unresolved
FROM comments
WHERE org_id = $1 AND node_id = $2 AND resolved_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;
