-- Organizations are the tenancy root: the one table with no org_id to
-- scope by, because its id is the scope.

-- name: CreateOrganization :one
INSERT INTO organizations (id, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetOrganization :one
SELECT * FROM organizations
WHERE id = $1;

-- Every organization, for work the service does across the deployment
-- rather than for one caller, such as the lease reaper.
-- name: ListOrganizations :many
SELECT * FROM organizations
ORDER BY id;

-- The organization bootstrap adopts before creating one. Names are not
-- unique, so the oldest wins; bootstrap serializes itself to keep it so.
-- name: GetOrganizationByName :one
SELECT * FROM organizations
WHERE name = $1
ORDER BY id
LIMIT 1;
