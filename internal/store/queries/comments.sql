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
