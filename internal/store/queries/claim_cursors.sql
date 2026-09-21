-- name: GetClaimCursor :one
SELECT * FROM claim_cursors
WHERE org_id = $1 AND principal_id = $2 AND filter_hash = $3;

-- A successful claim advances the cursor to the project it drew from; a
-- read only ever gets it, never upserts it.
-- name: UpsertClaimCursor :one
INSERT INTO claim_cursors (org_id, principal_id, filter_hash, project_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, principal_id, filter_hash)
DO UPDATE SET project_id = EXCLUDED.project_id, updated_at = now()
RETURNING *;
