-- name: CreateIdempotencyKey :one
INSERT INTO idempotency_keys (org_id, request_id, request_hash, response)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE org_id = $1 AND request_id = $2;

-- name: DeleteIdempotencyKeysBefore :execrows
DELETE FROM idempotency_keys
WHERE org_id = $1 AND created_at < $2;

-- A patch claims its key before doing any work and stores the response
-- once it has one, so a concurrent repeat blocks on the key row rather
-- than repeating the work.
-- name: SetIdempotencyKeyResponse :one
UPDATE idempotency_keys
SET response = $3
WHERE org_id = $1 AND request_id = $2
RETURNING *;
