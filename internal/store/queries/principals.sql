-- name: CreatePrincipal :one
INSERT INTO principals (id, org_id, kind)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetPrincipal :one
SELECT * FROM principals
WHERE org_id = $1 AND id = $2;

-- name: AddPrincipalIdentity :one
INSERT INTO principal_identities (principal_id, scheme, value)
SELECT p.id, $3, $4 FROM principals AS p
WHERE p.org_id = $1 AND p.id = $2
RETURNING *;

-- The transport's question: which principal presented this identity? An
-- identity is unique across the deployment, so this is the one read that
-- starts without an organization and returns one.
-- name: GetPrincipalByIdentity :one
SELECT p.* FROM principals AS p
JOIN principal_identities AS i ON i.principal_id = p.id
WHERE i.scheme = $1 AND i.value = $2;
