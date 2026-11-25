package store

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNoRows is what a query that returns one row reports when it finds
// none. It is pgx's own value, so errors.Is matches it however the error
// was wrapped on its way out.
var ErrNoRows = pgx.ErrNoRows

// uniqueViolation is the SQLSTATE PostgreSQL reports when an insert or
// update would duplicate a key a unique constraint or index guards.
const uniqueViolation = "23505"

// IsIdempotencyKeyConflict reports that a write collided with an
// idempotency key another transaction committed first.
func IsIdempotencyKeyConflict(err error) bool {
	return isUniqueViolation(err, "idempotency_keys_pkey")
}

// IsLiveLeaseConflict reports that a lease could not be created because
// its node already has a live one: leases_live_idx allows one unreleased
// lease per node.
func IsLiveLeaseConflict(err error) bool {
	return isUniqueViolation(err, "leases_live_idx")
}

// IsIdentityConflict reports that an identity could not be bound because
// another principal already holds that scheme and value.
func IsIdentityConflict(err error) bool {
	return isUniqueViolation(err, "principal_identities_pkey")
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
