-- name: CreateProject :one
INSERT INTO projects (id, org_id, slug, title, priority, vcs_url)
VALUES ($1, $2, $3, $4, $5, $6)
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
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);

-- name: RenameProject :one
UPDATE projects
SET slug = $3, title = $4
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: SetProjectVCSURL :one
UPDATE projects
SET vcs_url = $3
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

-- Parks a project: every node in it leaves the ready set, and unlike
-- archiving the project stays writable.
-- name: HoldProject :one
UPDATE projects
SET held_at = now(), held_by = $3, held_reason = $4
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: UnholdProject :one
UPDATE projects
SET held_at = NULL, held_by = NULL, held_reason = NULL, hold_group = NULL
WHERE org_id = $1 AND id = $2
RETURNING *;

-- The projects a sweep would park: everything outside the keep-set that is
-- not archived and not already held.
-- name: HoldProjectsOutside :many
UPDATE projects
SET held_at = now(), held_by = $2, held_reason = $3, hold_group = $4
WHERE org_id = $1 AND archived_at IS NULL AND held_at IS NULL
  AND NOT (id = ANY(sqlc.arg(keep)::text[]))
RETURNING *;

-- name: ReleaseProjectHoldGroup :many
UPDATE projects
SET held_at = NULL, held_by = NULL, held_reason = NULL, hold_group = NULL
WHERE org_id = $1 AND hold_group = $2
RETURNING *;

-- The sweeps with live project holds, so the active one can be found
-- without a caller remembering its id.
-- name: ProjectHoldGroups :many
SELECT DISTINCT hold_group::text AS hold_group FROM projects
WHERE org_id = $1 AND hold_group IS NOT NULL
ORDER BY hold_group;
