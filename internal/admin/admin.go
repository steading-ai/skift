// Package admin operates a deployment: it applies migrations, bootstraps
// the first principal, opens the database for the service, and will run
// skift's own background jobs. It reaches the store directly, because none
// of this is a rule of the graph; handlers never import it. See
// docs/adrs/016-background-jobs-and-admin.md.
package admin

import (
	"context"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/store/migrations"
)

// MigrationReport is what one [ApplyMigrations] did.
type MigrationReport = migrations.Report

// SchemaStatus is where a database stands against the schema the binary
// carries.
type SchemaStatus = migrations.Status

// ErrDatabaseUnreachable is what [ApplyMigrations] and [InspectSchema]
// fail with when the database does not answer, as opposed to answering
// and refusing.
var ErrDatabaseUnreachable = migrations.ErrUnreachable

// Migrate brings the database at databaseURL to the current schema. It has
// the shape a spec harness hands to testsupport.Start.
func Migrate(ctx context.Context, databaseURL string) error {
	return migrations.Migrate(ctx, databaseURL)
}

// ApplyMigrations is [Migrate] reporting which migrations it applied. It
// refuses a database migrated by a newer skift.
func ApplyMigrations(ctx context.Context, databaseURL string) (MigrationReport, error) {
	return migrations.Apply(ctx, databaseURL)
}

// InspectSchema reads the schema version of the database without changing
// it, for a caller that must refuse a database that is behind or ahead.
func InspectSchema(ctx context.Context, databaseURL string) (SchemaStatus, error) {
	return migrations.Inspect(ctx, databaseURL)
}

// Open connects to the database at databaseURL, for a composition root
// that runs the domain and does not name pgx. The caller closes what it
// returns; until then the pool connects lazily, and its Ping reports a
// database that is down.
func Open(ctx context.Context, databaseURL string) (*store.Store, error) {
	return store.Open(ctx, databaseURL)
}
