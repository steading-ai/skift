-- name: CreateBlob :one
INSERT INTO blobs (org_id, digest, media_type, size, backend)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT ON CONSTRAINT blobs_pkey DO UPDATE SET digest = excluded.digest
RETURNING *;

-- name: GetBlob :one
SELECT * FROM blobs
WHERE org_id = $1 AND digest = $2;

-- name: CreateArtifact :one
INSERT INTO artifacts (id, org_id, node_id, name, description, metadata, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetArtifact :one
SELECT * FROM artifacts
WHERE org_id = $1 AND id = $2;

-- name: ListArtifactsForNode :many
SELECT * FROM artifacts
WHERE org_id = $1 AND node_id = $2
ORDER BY created_at, id;

-- name: CreateArtifactPart :one
INSERT INTO artifact_parts (org_id, artifact_id, position, kind, text, blob_digest, reference_uri, reference_digest, media_type, size)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: ListArtifactParts :many
SELECT * FROM artifact_parts
WHERE org_id = $1 AND artifact_id = $2
ORDER BY position;
