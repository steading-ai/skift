package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// How [Store.Snapshot] copied a table out, so that restoring it can
// reverse the copy.
const (
	// CopyTable is `COPY name (columns) TO STDOUT`: the table's own rows.
	CopyTable = "table"
	// CopyQuery is `COPY (SELECT columns FROM name) TO STDOUT`, for a
	// partitioned table, which COPY TO does not take by name. Its rows
	// are those of every partition, and restoring it with COPY FROM routes
	// each to its partition.
	CopyQuery = "query"
)

// snapshotLockTimeout bounds how long a snapshot waits to lock the tables
// it copies, so one that cannot get them, behind a migration say, fails
// and is tried again rather than hanging the upgrade that asked for it.
const snapshotLockTimeout = 30 * time.Second

// snapshotSettings fix the text COPY writes, so that what one session
// writes another reads back the same whatever the server's defaults:
// dates in ISO order, timestamps in UTC, floats exact. A restore sets
// them before its COPY FROM. These are pg_dump's choices; see
// https://www.postgresql.org/docs/current/runtime-config-client.html.
var snapshotSettings = [][2]string{
	{"DateStyle", "ISO, MDY"},
	{"IntervalStyle", "postgres"},
	{"TimeZone", "UTC"},
	{"extra_float_digits", "3"},
	{"bytea_output", "hex"},
}

// Snapshot describes one consistent copy of every table in skift's
// schema, as [Store.Snapshot] took it.
type Snapshot struct {
	// SchemaVersion is the newest migration applied when the copy began.
	SchemaVersion int64
	// TakenAt is when the copy's transaction began.
	TakenAt time.Time
	// ServerVersion is the PostgreSQL server's version string.
	ServerVersion string
	// Settings are the session settings the COPY text was written under,
	// by name, in the order they were set.
	Settings [][2]string
	// Tables is every table copied, each after the tables its foreign
	// keys reference.
	Tables []SnapshotTable
	// Sequences is every sequence in the schema, by name.
	Sequences []SnapshotSequence
}

// SnapshotTable is one table as [Store.Snapshot] copied it.
type SnapshotTable struct {
	// Name is the table's name in skift's schema, unqualified.
	Name string
	// Columns names the columns copied, in the order each row holds
	// them: every stored column, in table order, leaving out generated
	// ones, which COPY FROM does not take.
	Columns []string
	// Rows is how many rows the COPY wrote.
	Rows int64
	// Copy is [CopyTable] or [CopyQuery].
	Copy string
	// Partitions names a partitioned table's partitions with their
	// bounds, so a restore can create each before copying rows into it.
	// Empty for a table that is not partitioned.
	Partitions []SnapshotPartition
}

// SnapshotPartition is one partition of a partitioned table.
type SnapshotPartition struct {
	// Name is the partition's table name.
	Name string
	// Bound is its partition bound as PostgreSQL prints it, such as
	// `FOR VALUES FROM ('2026-11-01 00:00:00+00') TO ('2026-12-01 00:00:00+00')`.
	Bound string
}

// SnapshotSequence is one sequence's position when the copy was taken.
// Sequences are not transactional, so it may be ahead of every row the
// copy holds, never behind; a restore sets it with setval.
type SnapshotSequence struct {
	Name      string
	LastValue int64
	IsCalled  bool
}

// ErrNoSchema is what [Store.Snapshot] fails with on a database that skift
// has never migrated: there is nothing of skift's in it to copy.
var ErrNoSchema = errors.New("the database has no skift schema: no migration has been applied")

// SchemaVersion reads the newest migration applied to the database, 0 when
// none is, without locking or creating anything. It is the version goose
// reports, read from goose's own table.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("connect to database: %w", err)
	}
	defer conn.Release()
	return schemaVersion(ctx, conn.Conn())
}

// querier is what both a connection and a transaction run a query on.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// schemaVersion is goose's version: the greatest version_id in its table,
// which goose names [goose.DefaultTablename] since the migrations package
// leaves the name at its default.
func schemaVersion(ctx context.Context, q querier) (int64, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, goose.DefaultTablename).Scan(&exists); err != nil {
		return 0, fmt.Errorf("look for the migration table: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var version int64
	err := q.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM `+pgx.Identifier{goose.DefaultTablename}.Sanitize()).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// Snapshot copies every table in skift's schema out with COPY, inside one
// REPEATABLE READ READ ONLY transaction, so the copy is of one moment while
// the service goes on writing. The tables are found in the catalogue, not
// listed by hand, so a table a migration adds is never left out: every
// ordinary and partitioned table in the connection's current schema,
// River's and goose's included. A partition is copied as part of its
// parent.
//
// For each table, in [Snapshot.Tables] order, Snapshot calls open with the
// table's name and writes the table's rows to what it returns, in COPY's
// text format under [Snapshot.Settings]. Nothing about the copy is
// returned until every table has been written; an error from open, or
// from writing to what it returned, ends the snapshot with that error.
//
// No statement timeout applies, since a large table may take long to
// copy, but taking the tables' locks gives up after [snapshotLockTimeout].
// A database skift has never migrated fails with [ErrNoSchema].
func (s *Store) Snapshot(ctx context.Context, open func(table string) (io.Writer, error)) (Snapshot, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("connect to database: %w", err)
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin transaction: %w", err)
	}
	// The transaction only reads, so ending it without a commit loses
	// nothing; a rollback on a broken connection has nothing to report.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// SET takes no bind parameters. Every value here is a constant of
	// this file, and none needs quoting beyond the single quotes added.
	settings := append([][2]string{
		{"statement_timeout", "0"},
		{"lock_timeout", fmt.Sprint(snapshotLockTimeout.Milliseconds())},
	}, snapshotSettings...)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, "SET LOCAL "+setting[0]+" = '"+setting[1]+"'"); err != nil {
			return Snapshot{}, fmt.Errorf("set %s: %w", setting[0], err)
		}
	}

	// This is the transaction's first query, so it fixes the snapshot
	// every read after it sees.
	snap := Snapshot{Settings: slices.Clone(snapshotSettings)}
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp(), current_setting('server_version')`).Scan(&snap.TakenAt, &snap.ServerVersion); err != nil {
		return Snapshot{}, fmt.Errorf("begin the snapshot: %w", err)
	}
	if snap.SchemaVersion, err = schemaVersion(ctx, tx); err != nil {
		return Snapshot{}, err
	}
	if snap.SchemaVersion == 0 {
		return Snapshot{}, ErrNoSchema
	}

	tables, err := snapshotTables(ctx, tx)
	if err != nil {
		return Snapshot{}, err
	}
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = pgx.Identifier{t.Name}.Sanitize()
	}
	// Holding every table's lock from here on keeps a migration from
	// changing one the copy has yet to reach.
	if _, err := tx.Exec(ctx, `LOCK TABLE `+strings.Join(names, ", ")+` IN ACCESS SHARE MODE`); err != nil {
		return Snapshot{}, fmt.Errorf("lock the tables: %w", err)
	}

	for i := range tables {
		t := &tables[i]
		w, err := open(t.Name)
		if err != nil {
			return Snapshot{}, err
		}
		columns := make([]string, len(t.Columns))
		for j, c := range t.Columns {
			columns[j] = pgx.Identifier{c}.Sanitize()
		}
		sql := `COPY ` + names[i] + ` (` + strings.Join(columns, ", ") + `) TO STDOUT`
		if t.Copy == CopyQuery {
			sql = `COPY (SELECT ` + strings.Join(columns, ", ") + ` FROM ` + names[i] + `) TO STDOUT`
		}
		tag, err := tx.Conn().PgConn().CopyTo(ctx, w, sql)
		if err != nil {
			return Snapshot{}, fmt.Errorf("copy %s: %w", t.Name, err)
		}
		t.Rows = tag.RowsAffected()
	}
	snap.Tables = tables

	if snap.Sequences, err = snapshotSequences(ctx, tx); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// snapshotTables finds every table to copy, with its columns and
// partitions, ordered so that each follows the tables it references.
func snapshotTables(ctx context.Context, tx pgx.Tx) ([]SnapshotTable, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.relname,
		       c.relkind = 'p',
		       array(SELECT a.attname::text FROM pg_attribute a
		              WHERE a.attrelid = c.oid AND a.attnum > 0
		                AND NOT a.attisdropped AND a.attgenerated = ''
		              ORDER BY a.attnum),
		       array(SELECT p.relname::text FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhrelid
		              WHERE i.inhparent = c.oid ORDER BY p.relname),
		       array(SELECT pg_get_expr(p.relpartbound, p.oid) FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhrelid
		              WHERE i.inhparent = c.oid ORDER BY p.relname),
		       array(SELECT DISTINCT r.relname::text FROM pg_constraint f JOIN pg_class r ON r.oid = f.confrelid
		              WHERE f.conrelid = c.oid AND f.contype = 'f' AND f.confrelid <> c.oid)
		  FROM pg_class c
		 WHERE c.relnamespace = current_schema()::regnamespace
		   AND c.relkind IN ('r', 'p')
		   AND NOT c.relispartition
		 ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var (
		tables     []SnapshotTable
		references = map[string][]string{}
	)
	for rows.Next() {
		var (
			t           SnapshotTable
			partitioned bool
			parts       []string
			bounds      []string
			refs        []string
		)
		if err := rows.Scan(&t.Name, &partitioned, &t.Columns, &parts, &bounds, &refs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list tables: %w", err)
		}
		t.Copy = CopyTable
		if partitioned {
			t.Copy = CopyQuery
		}
		for i := range parts {
			t.Partitions = append(t.Partitions, SnapshotPartition{Name: parts[i], Bound: bounds[i]})
		}
		references[t.Name] = refs
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return referencedFirst(tables, references), nil
}

// referencedFirst orders tables so that each comes after every table it
// references, taking the first in name order whenever several are free.
// A reference to a table outside the list, or to itself, places nothing.
// Tables caught in a cycle of references follow the rest, in name order.
func referencedFirst(tables []SnapshotTable, references map[string][]string) []SnapshotTable {
	remaining := slices.Clone(tables)
	slices.SortFunc(remaining, func(a, b SnapshotTable) int { return cmp.Compare(a.Name, b.Name) })
	placed := map[string]bool{}
	listed := map[string]bool{}
	for _, t := range tables {
		listed[t.Name] = true
	}
	ordered := make([]SnapshotTable, 0, len(tables))
	for len(remaining) > 0 {
		next := slices.IndexFunc(remaining, func(t SnapshotTable) bool {
			for _, r := range references[t.Name] {
				if listed[r] && !placed[r] {
					return false
				}
			}
			return true
		})
		if next < 0 {
			return append(ordered, remaining...)
		}
		placed[remaining[next].Name] = true
		ordered = append(ordered, remaining[next])
		remaining = slices.Delete(remaining, next, next+1)
	}
	return ordered
}

// snapshotSequences reads the position of every sequence in the schema.
func snapshotSequences(ctx context.Context, tx pgx.Tx) ([]SnapshotSequence, error) {
	rows, err := tx.Query(ctx, `
		SELECT relname::text FROM pg_class
		 WHERE relnamespace = current_schema()::regnamespace AND relkind = 'S'
		 ORDER BY relname`)
	if err != nil {
		return nil, fmt.Errorf("list sequences: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list sequences: %w", err)
	}
	sequences := make([]SnapshotSequence, len(names))
	for i, name := range names {
		sequences[i].Name = name
		err := tx.QueryRow(ctx, `SELECT last_value, is_called FROM `+pgx.Identifier{name}.Sanitize()).
			Scan(&sequences[i].LastValue, &sequences[i].IsCalled)
		if err != nil {
			return nil, fmt.Errorf("read sequence %s: %w", name, err)
		}
	}
	return sequences, nil
}
