-- LockBootstrap is the lock two bootstraps of one database serialize on,
-- so that a race creates one organization rather than two carrying the
-- same name. The key is a fixed number the caller owns.
-- name: LockBootstrap :exec
SELECT pg_advisory_xact_lock($1);

-- LockNamespaced takes the advisory lock keyed by a namespace and a key
-- within it, so one use of advisory locks cannot collide with another. It
-- blocks until any holder commits or rolls back.
-- name: LockNamespaced :exec
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(namespace)), hashtext(sqlc.arg(key)));
