-- name: CreateProject :one
INSERT INTO projects (id, org_id, slug, title, priority)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects
WHERE org_id = $1 AND id = $2;

-- Resolves a slug whether it is live or retired: the lookup behind
-- `slug#7` in a reference.
-- name: GetProjectBySlug :one
SELECT p.* FROM projects AS p
JOIN project_slugs AS s ON s.org_id = p.org_id AND s.project_id = p.id
WHERE p.org_id = $1 AND s.slug = $2;

-- Most recently created first. Archived projects are hidden unless asked
-- for.
-- name: ListProjects :many
SELECT * FROM projects
WHERE org_id = $1
  AND (archived_at IS NULL OR sqlc.arg(include_archived)::boolean)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: RenameProject :one
UPDATE projects
SET slug = $3, title = $4
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: ArchiveProject :one
UPDATE projects
SET archived_at = coalesce(archived_at, now())
WHERE org_id = $1 AND id = $2
RETURNING *;

-- Hands out a block of node numbers in one statement: the first number of
-- the block is returned and the counter moves past it. See the Keys
-- section of docs/domain-model.md.
-- name: AllocateNodeNumbers :one
UPDATE projects
SET node_counter = node_counter + sqlc.arg(count)::bigint
WHERE org_id = $1 AND id = $2
RETURNING node_counter - sqlc.arg(count)::bigint AS first_number;
