-- name: CreateLease :one
INSERT INTO leases (id, org_id, node_id, holder, on_behalf_of, harness, agent_ref, session_ref, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetLease :one
SELECT * FROM leases
WHERE org_id = $1 AND id = $2;

-- The unreleased lease on a node, if any. Callers compare expires_at with
-- the clock to tell live from expired.
-- name: GetUnreleasedLease :one
SELECT * FROM leases
WHERE org_id = $1 AND node_id = $2 AND released_at IS NULL;

-- name: ListLeasesForNode :many
SELECT * FROM leases
WHERE org_id = $1 AND node_id = $2
ORDER BY claimed_at DESC, id DESC;

-- name: ListUnreleasedLeasesForHolder :many
SELECT * FROM leases
WHERE org_id = $1 AND holder = $2 AND released_at IS NULL
ORDER BY claimed_at, id;

-- Fenced renewal: only the named lease, only while it is live.
-- name: RenewLease :one
UPDATE leases
SET expires_at = $3
WHERE org_id = $1 AND id = $2 AND released_at IS NULL AND expires_at > now()
RETURNING *;

-- name: ReleaseLease :one
UPDATE leases
SET released_at = now(), release_reason = $3
WHERE org_id = $1 AND id = $2 AND released_at IS NULL
RETURNING *;

-- Unreleased leases whose TTL has passed, for the expiry timer.
-- name: ListExpiredLeases :many
SELECT * FROM leases
WHERE org_id = $1 AND released_at IS NULL AND expires_at <= now()
ORDER BY expires_at, id
LIMIT sqlc.arg(page_size);
