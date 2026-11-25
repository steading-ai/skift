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

// Open connects to the database at databaseURL without checking its
// schema, for serve: it opens before the database may answer and checks
// the schema itself once it does. The caller closes what it returns; until
// then the pool connects lazily, and its Ping reports a database that is
// down. Every other command uses [OpenCurrent], and lint forbids calling
// Open anywhere but serve.
func Open(ctx context.Context, databaseURL string) (*store.Store, error) {
	return store.Open(ctx, databaseURL)
}

// OpenCurrent connects to the database at databaseURL for a command that
// acts on it once and exits, such as bootstrap or the jobs commands. It
// reads the schema version first and refuses, before anything is read or
// written, a database migrated by a newer skift or not yet migrated to
// this build's schema. See docs/adrs/015-upgrades-and-migrations.md.
func OpenCurrent(ctx context.Context, databaseURL string) (*store.Store, error) {
	status, err := migrations.Inspect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := status.Check(); err != nil {
		return nil, err
	}
	return store.Open(ctx, databaseURL)
}
