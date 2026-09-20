// Package migrations holds the schema as goose SQL migrations and applies
// them. The files are embedded so the binary carries its own schema, and sqlc
// reads the same files, so the two never disagree.
//
// Migrations are the only way the schema changes. Each one has a down that
// leaves the database exactly as its up found it, and the specs prove it.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// FS holds every migration, in goose's `NNNNN_name.sql` layout.
//
//go:embed *.sql
var FS embed.FS

// ErrUnreachable wraps the failure of [Open] to reach the database, so a
// caller can tell a server that is not yet answering from a migration that
// failed, and wait for the one without retrying the other.
var ErrUnreachable = errors.New("database unreachable")

// Migrator applies migrations to one database.
type Migrator struct {
	provider *goose.Provider
}

// Open connects to databaseURL and prepares a Migrator over [FS]. Close it
// when done.
func Open(ctx context.Context, databaseURL string) (*Migrator, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS, goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return nil, joinClose(fmt.Errorf("prepare migrations: %w", err), db)
	}
	if err := provider.Ping(ctx); err != nil {
		return nil, joinClose(fmt.Errorf("connect to database: %w: %w", ErrUnreachable, err), db)
	}
	return &Migrator{provider: provider}, nil
}

// Close releases the connection.
func (m *Migrator) Close() error {
	return m.provider.Close()
}

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) error {
	_, err := m.Apply(ctx)
	return err
}

// Apply applies every pending migration and returns the versions it
// applied, oldest first; none when the database was already current.
func (m *Migrator) Apply(ctx context.Context) ([]int64, error) {
	results, err := m.provider.Up(ctx)
	applied := make([]int64, 0, len(results))
	for _, r := range results {
		if r.Error == nil {
			applied = append(applied, r.Source.Version)
		}
	}
	return applied, err
}

// UpTo applies pending migrations up to and including version.
func (m *Migrator) UpTo(ctx context.Context, version int64) error {
	_, err := m.provider.UpTo(ctx, version)
	return err
}

// DownTo rolls back migrations newer than version, newest first. Version 0
// rolls back everything.
func (m *Migrator) DownTo(ctx context.Context, version int64) error {
	_, err := m.provider.DownTo(ctx, version)
	return err
}

// Version reports the newest applied migration, or 0 on an empty database.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	return m.provider.GetDBVersion(ctx)
}

// Latest is the newest migration in [FS]: the version a current database
// is at.
func (m *Migrator) Latest() int64 {
	versions := m.Versions()
	if len(versions) == 0 {
		return 0
	}
	return versions[len(versions)-1]
}

// Versions lists every migration in [FS], oldest first.
func (m *Migrator) Versions() []int64 {
	sources := m.provider.ListSources()
	versions := make([]int64, 0, len(sources))
	for _, s := range sources {
		versions = append(versions, s.Version)
	}
	return versions
}

// Migrate brings the database at databaseURL up to the current schema. It has
// the shape testsupport.Migrate expects, so a suite can hand it straight to
// testsupport.Start.
func Migrate(ctx context.Context, databaseURL string) error {
	m, err := Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	if err := m.Up(ctx); err != nil {
		return joinClose(fmt.Errorf("apply migrations: %w", err), m)
	}
	return m.Close()
}

// Report is what one [Apply] did to a database.
type Report struct {
	// Applied is every version this run applied, oldest first. Empty
	// when the database was already at Version.
	Applied []int64 `json:"applied"`
	// Version is the schema version the database is at afterwards.
	Version int64 `json:"version"`
}

// Apply brings the database at databaseURL up to the current schema and
// reports what that took. Running it on a current database applies
// nothing and is not an error. A database that does not answer fails with
// [ErrUnreachable].
func Apply(ctx context.Context, databaseURL string) (Report, error) {
	m, err := Open(ctx, databaseURL)
	if err != nil {
		return Report{}, err
	}
	applied, err := m.Apply(ctx)
	if err != nil {
		return Report{}, joinClose(fmt.Errorf("apply migrations: %w", err), m)
	}
	version, err := m.Version(ctx)
	if err != nil {
		return Report{}, joinClose(fmt.Errorf("read schema version: %w", err), m)
	}
	return Report{Applied: applied, Version: version}, m.Close()
}

// Status is where a database's schema stands against the migrations the
// binary carries.
type Status struct {
	// Current is the newest migration applied to the database; 0 when
	// none is.
	Current int64
	// Latest is the newest migration in [FS].
	Latest int64
}

// Pending reports whether the database is behind the binary.
func (s Status) Pending() bool { return s.Current < s.Latest }

// Inspect reads the schema version of the database at databaseURL without
// changing it. A database that does not answer fails with [ErrUnreachable].
func Inspect(ctx context.Context, databaseURL string) (Status, error) {
	m, err := Open(ctx, databaseURL)
	if err != nil {
		return Status{}, err
	}
	current, err := m.Version(ctx)
	if err != nil {
		return Status{}, joinClose(fmt.Errorf("read schema version: %w", err), m)
	}
	return Status{Current: current, Latest: m.Latest()}, m.Close()
}

type closer interface{ Close() error }

// joinClose closes c after a failure, keeping err as the primary error.
func joinClose(err error, c closer) error {
	if cerr := c.Close(); cerr != nil {
		return fmt.Errorf("%w (and close: %w)", err, cerr)
	}
	return err
}
