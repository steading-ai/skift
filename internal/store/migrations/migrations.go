// Package migrations holds the schema as goose SQL migrations and applies
// them. The files are embedded so the binary carries its own schema, and sqlc
// reads the same files, so the two never disagree.
//
// Migrations are the only way the schema changes. Each one has a down that
// leaves the database exactly as its up found it, and the specs prove it.
package migrations

import (
	"context"
	"database/sql"
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
	db          *sql.DB
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
	return &Migrator{provider: provider, db: db, retryBudget: RetryBudget}, nil
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
// It refuses a database whose floor is above the binary's newest
// migration with a [*SchemaFloorError], before applying anything. A
// database migrated past the binary within its floor has nothing pending,
// so Apply applies nothing to it. A migration that hits [LockTimeout]
// rolls back alone, since goose runs each in its own transaction; Apply
// then backs off and runs again, which recomputes what is pending, until
// the run succeeds or [RetryBudget] is spent.
func (m *Migrator) Apply(ctx context.Context) ([]int64, error) {
	status, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	if err := status.Servable(); err != nil {
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

// Status reads where the database's schema stands against the migrations
// the binary carries, its floor included.
func (m *Migrator) Status(ctx context.Context) (Status, error) {
	current, err := m.Version(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("read schema version: %w", err)
	}
	floor, err := m.floor(ctx, current)
	if err != nil {
		return Status{}, fmt.Errorf("read schema floor: %w", err)
	}
	return Status{Current: current, Latest: m.Latest(), Floor: floor}, nil
}

// floor reads the schema's compatibility floor from schema_floor. A schema
// that predates the table records none, and needs a binary that carries
// its newest migration, current.
func (m *Migrator) floor(ctx context.Context, current int64) (int64, error) {
	var recorded bool
	if err := m.db.QueryRowContext(ctx, `SELECT to_regclass('schema_floor') IS NOT NULL`).Scan(&recorded); err != nil {
		return 0, err
	}
	if !recorded {
		return current, nil
	}
	var floor int64
	if err := m.db.QueryRowContext(ctx, `SELECT version FROM schema_floor`).Scan(&floor); err != nil {
		return 0, err
	}
	return floor, nil
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
	// Floor is the schema's compatibility floor: the lowest migration a
	// binary must carry to serve it. A schema that records none has its
	// Current as its floor. See docs/adrs/018-unattended-upgrades.md.
	Floor int64
}

// Pending reports whether the database is behind the binary.
func (s Status) Pending() bool { return s.Current < s.Latest }

// Ahead reports whether the database carries migrations the binary does
// not: it was migrated by a newer skift. goose ignores versions above its
// own newest, so an ahead schema has nothing pending, and only its floor
// says whether this binary may serve it.
func (s Status) Ahead() bool { return s.Current > s.Latest }

// Servable is nil when the binary carries the schema's floor, whether or
// not the schema is ahead of the binary, and otherwise a
// [*SchemaFloorError] naming the floor and the binary's newest migration.
func (s Status) Servable() error {
	if s.Floor > s.Latest {
		return &SchemaFloorError{Current: s.Current, Floor: s.Floor, Latest: s.Latest}
	}
	return nil
}

// SchemaFloorError refuses a database migrated by a newer skift past what
// this binary can serve: its floor is above the binary's newest migration.
// The way back is a skift that carries the floor, or a snapshot taken
// before the upgrade. See docs/adrs/018-unattended-upgrades.md.
type SchemaFloorError struct {
	// Current is the database's schema version.
	Current int64
	// Floor is the lowest migration a binary must carry to serve it.
	Floor int64
	// Latest is the newest migration this binary carries.
	Latest int64
}

func (e *SchemaFloorError) Error() string {
	return fmt.Sprintf("schema %d needs migration %d, and this build's newest is %d: install skift that carries migration %d, or restore a snapshot", e.Current, e.Floor, e.Latest, e.Floor)
}

// Check is nil when the binary can act on the database as it stands: the
// binary carries the schema's floor, and the database is not behind. A
// schema whose floor is above the binary is a [*SchemaFloorError], and one
// not yet migrated to this build's schema is a [*SchemaBehindError]. It is
// the check for a command that acts on the database without migrating it.
func (s Status) Check() error {
	if err := s.Servable(); err != nil {
		return err
	}
	if s.Pending() {
		return &SchemaBehindError{Current: s.Current, Latest: s.Latest}
	}
	return nil
}

// SchemaBehindError refuses a database that has not been migrated to this
// build's schema, for a command that would act on it without migrating.
type SchemaBehindError struct {
	// Current is the database's schema version.
	Current int64
	// Latest is the newest migration this binary carries.
	Latest int64
}

func (e *SchemaBehindError) Error() string {
	return fmt.Sprintf("schema %d is behind this build's %d: run skift admin migrate", e.Current, e.Latest)
}

// Inspect reads the schema version and floor of the database at
// databaseURL without changing it. A database that does not answer fails
// with [ErrUnreachable].
func Inspect(ctx context.Context, databaseURL string) (Status, error) {
	m, err := Open(ctx, databaseURL)
	if err != nil {
		return Status{}, err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return Status{}, joinClose(err, m)
	}
	return status, m.Close()
}

type closer interface{ Close() error }

// joinClose closes c after a failure, keeping err as the primary error.
func joinClose(err error, c closer) error {
	if cerr := c.Close(); cerr != nil {
		return fmt.Errorf("%w (and close: %w)", err, cerr)
	}
	return err
}
