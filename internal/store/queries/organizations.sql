-- Organizations are the tenancy root: the one table with no org_id to
-- scope by, because its id is the scope.

-- name: CreateOrganization :one
INSERT INTO organizations (id, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations
WHERE id = $1;
