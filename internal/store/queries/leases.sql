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

-- The named lease only while it is live: unreleased, and its TTL not yet
-- passed by the server clock. The fencing lookup a write starts with.
-- name: GetLiveLease :one
SELECT * FROM leases
WHERE org_id = $1 AND id = $2 AND released_at IS NULL AND expires_at > now();

-- Implicit renewal: a write by the holder moves the expiry out to the given
-- time and never back, so a long explicit TTL survives a write. Fenced like
-- RenewLease.
-- name: ExtendLease :one
UPDATE leases
SET expires_at = GREATEST(expires_at, sqlc.arg(expires_at)::timestamptz)
WHERE org_id = $1 AND id = $2 AND released_at IS NULL AND expires_at > now()
RETURNING *;

-- Fenced release: the named lease, only while it is live. A lease whose TTL
-- has passed is released as expired by ExpireLeases and by nothing else.
-- name: ReleaseLiveLease :one
UPDATE leases
SET released_at = now(), release_reason = $3
WHERE org_id = $1 AND id = $2 AND released_at IS NULL AND expires_at > now()
RETURNING *;

-- Releases the given leases as expired, when their TTL has passed by the
-- time the statement runs. A lease renewed since it was listed is left
-- alone.
-- name: ExpireLeases :many
UPDATE leases
SET released_at = now(), release_reason = 'expired'
WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::text[]) AND released_at IS NULL AND expires_at <= now()
RETURNING *;

-- Releases the expired lease on one node, if there is one, so a claim can
-- take the node before the expiry timer has been round.
-- name: ExpireLeasesForNode :many
UPDATE leases
SET released_at = now(), release_reason = 'expired'
WHERE org_id = $1 AND node_id = $2 AND released_at IS NULL AND expires_at <= now()
RETURNING *;
