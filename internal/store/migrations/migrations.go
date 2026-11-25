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
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// FS holds every migration, in goose's `NNNNN_name.sql` layout.
//
//go:embed *.sql
var FS embed.FS

// ErrUnreachable wraps the failure of [Open] to reach the database, so a
// caller can tell a server that is not yet answering from a migration that
// failed, and wait for the one without retrying the other.
var ErrUnreachable = errors.New("database unreachable")

// LockTimeout is how long one migration statement may wait for a lock
// before PostgreSQL cancels it with SQLSTATE 55P03. A statement waiting for
// a table's ACCESS EXCLUSIVE lock blocks every query queued behind it, so
// the wait is kept short and the run retried rather than left to stall
// the service. It is set on the migration connection, so it covers
// transactional and NO TRANSACTION migrations alike. See
// docs/adrs/015-upgrades-and-migrations.md.
const LockTimeout = 2 * time.Second

// RetryBudget bounds the time [Migrator.Apply] spends retrying after lock
// timeouts. Together with the migrations themselves it must fit inside
// serve's start-up, which systemd bounds at 90 seconds by default, so a
// run that cannot get its locks fails with time to report why.
const RetryBudget = 60 * time.Second

// lockID is the session advisory lock that admits one migrating process at
// a time. It is skift's own rather than goose's default, so another goose
// user in the same database never waits on skift: the bytes of "skift".
const lockID int64 = 0x736b696674

// lockWaitProbes is how many one-second probes a process makes for the
// migration lock before giving up: as long as another process's run may
// take, retries included.
const lockWaitProbes = 60

// lockNotAvailable is PostgreSQL's SQLSTATE for a lock_timeout expiring.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const lockNotAvailable = "55P03"

// Migrator applies migrations to one database.
type Migrator struct {
	provider    *goose.Provider
	retryBudget time.Duration
	// lockTimeouts counts the lock timeouts the last Apply retried after,
	// for the specs.
	lockTimeouts int
}

// Open connects to databaseURL and prepares a Migrator over [FS]. Close it
// when done.
//
// The connection runs with [LockTimeout] and no statement timeout: a
// migration may take long, but may not wait long for a lock. goose holds a
// session advisory lock for each run, so two processes never apply the
// same migration; PostgreSQL releases it when the session ends, so a
// process that dies mid-run leaves no stale lock.
func Open(ctx context.Context, databaseURL string) (*Migrator, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.RuntimeParams["lock_timeout"] = fmt.Sprintf("%dms", LockTimeout.Milliseconds())
	cfg.RuntimeParams["statement_timeout"] = "0"
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(lockID), lock.WithLockTimeout(1, lockWaitProbes))
	if err != nil {
		return nil, fmt.Errorf("prepare migration lock: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS,
		goose.WithLogger(goose.NopLogger()), goose.WithSessionLocker(locker))
	if err != nil {
		return nil, joinClose(fmt.Errorf("prepare migrations: %w", err), db)
	}
	if err := provider.Ping(ctx); err != nil {
		return nil, joinClose(fmt.Errorf("connect to database: %w: %w", ErrUnreachable, err), db)
	}
	return &Migrator{provider: provider, retryBudget: RetryBudget}, nil
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
//
// It refuses a database whose schema is newer than the binary's with a
// [*SchemaAheadError], before applying anything. A migration that hits
// [LockTimeout] rolls back alone, since goose runs each in its own
// transaction; Apply then backs off and runs again, which recomputes what
// is pending, until the run succeeds or [RetryBudget] is spent.
func (m *Migrator) Apply(ctx context.Context) ([]int64, error) {
	current, err := m.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if err := (Status{Current: current, Latest: m.Latest()}).NotAhead(); err != nil {
		return nil, err
	}
	m.lockTimeouts = 0
	deadline := time.Now().Add(m.retryBudget)
	applied := []int64{}
	for attempt := 0; ; attempt++ {
		results, err := m.provider.Up(ctx)
		for _, r := range results {
			if r.Error == nil {
				applied = append(applied, r.Source.Version)
			}
		}
		if err == nil || !isLockTimeout(err) {
			return applied, err
		}
		m.lockTimeouts++
		wait := backoff(attempt)
		if time.Now().Add(wait).After(deadline) {
			return applied, fmt.Errorf("gave up waiting for locks after %d tries within %s: %w", attempt+1, m.retryBudget, err)
		}
		select {
		case <-ctx.Done():
			return applied, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backoff is the wait before retry number attempt+1: exponential from a
// quarter second, capped at eight, with full jitter so that processes
// retrying together spread out.
func backoff(attempt int) time.Duration {
	ceiling := min(250*time.Millisecond<<min(attempt, 5), 8*time.Second)
	return time.Duration(rand.Int64N(int64(ceiling))) + time.Millisecond
}

// isLockTimeout reports whether err is a statement cancelled by
// lock_timeout.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
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

// Ahead reports whether the database carries migrations the binary does
// not: it was migrated by a newer skift. goose ignores versions above its
// own newest, so without this check an older binary would serve a schema
// it does not know.
func (s Status) Ahead() bool { return s.Current > s.Latest }

// NotAhead is nil unless the database is [Status.Ahead], when it is a
// [*SchemaAheadError] naming both versions.
func (s Status) NotAhead() error {
	if s.Ahead() {
		return &SchemaAheadError{Current: s.Current, Latest: s.Latest}
	}
	return nil
}

// SchemaAheadError refuses a database migrated by a newer skift. A
// downgrade means restoring the backup taken before the upgrade, so the
// error names that and the other way out. See
// docs/adrs/015-upgrades-and-migrations.md.
type SchemaAheadError struct {
	// Current is the database's schema version.
	Current int64
	// Latest is the newest migration this binary carries.
	Latest int64
}

func (e *SchemaAheadError) Error() string {
	return fmt.Sprintf("schema %d is newer than this build's %d: restore the pre-upgrade backup or install a newer skift", e.Current, e.Latest)
}

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
