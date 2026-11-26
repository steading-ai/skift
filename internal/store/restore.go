package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrRestoreMismatch is what [Store.Restore] fails with when the database
// or the rows copied into it do not match the snapshot it was given.
var ErrRestoreMismatch = errors.New("the database does not match the snapshot")

// SchemaContents is what the connection's current schema holds, leaving
// out the members of extensions.
type SchemaContents struct {
	// Tables names every ordinary and partitioned table that is not a
	// partition of another, in name order.
	Tables []string
	// Others counts the views, sequences, routines and types beside them.
	Others int
}

// Empty reports whether the schema holds nothing a migration would make.
func (c SchemaContents) Empty() bool { return len(c.Tables) == 0 && c.Others == 0 }

// schemaObjects lists what [Store.DropSchemaContents] drops, in an order
// that drops dependents first, each with the DROP that removes it and its
// name as SQL spells it. The kinds are those skift's migrations make:
// views, tables, sequences, routines, enums, ranges, domains and
// composite types. A partition goes with its table, a sequence a column
// owns with its column, and an array or row type with what it belongs to,
// so none is listed on its own.
const schemaObjects = `
	SELECT drop, name FROM (
		SELECT CASE c.relkind WHEN 'v' THEN 1 WHEN 'm' THEN 2 WHEN 'S' THEN 4 ELSE 3 END AS step,
		       CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE' ELSE 'TABLE' END AS drop,
		       c.oid::regclass::text AS name, c.oid AS oid, 'pg_class'::regclass AS catalog
		  FROM pg_class c
		 WHERE c.relnamespace = current_schema()::regnamespace
		   AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
		   AND NOT c.relispartition
		   AND NOT (c.relkind = 'S' AND EXISTS (
		                SELECT 1 FROM pg_depend d
		                 WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
		                   AND d.refobjsubid > 0 AND d.deptype IN ('a', 'i')))
		UNION ALL
		SELECT 5, 'ROUTINE', p.oid::regprocedure::text, p.oid, 'pg_proc'::regclass
		  FROM pg_proc p
		 WHERE p.pronamespace = current_schema()::regnamespace
		UNION ALL
		SELECT CASE t.typtype WHEN 'd' THEN 6 ELSE 7 END,
		       CASE t.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END,
		       t.oid::regtype::text, t.oid, 'pg_type'::regclass
		  FROM pg_type t
		  LEFT JOIN pg_class r ON r.oid = t.typrelid
		 WHERE t.typnamespace = current_schema()::regnamespace
		   AND (t.typtype IN ('e', 'r', 'd') OR (t.typtype = 'c' AND r.relkind = 'c'))
	) o
	WHERE NOT EXISTS (SELECT 1 FROM pg_depend e
	                   WHERE e.classid = o.catalog AND e.objid = o.oid AND e.deptype = 'e')
	ORDER BY step, name`

// SchemaContents reads what the connection's current schema holds,
// without locking or creating anything. A database skift has never
// migrated holds nothing.
func (s *Store) SchemaContents(ctx context.Context) (SchemaContents, error) {
	rows, err := s.pool.Query(ctx, schemaObjects)
	if err != nil {
		return SchemaContents{}, fmt.Errorf("list the schema's contents: %w", err)
	}
	var contents SchemaContents
	for rows.Next() {
		var drop, name string
		if err := rows.Scan(&drop, &name); err != nil {
			rows.Close()
			return SchemaContents{}, fmt.Errorf("list the schema's contents: %w", err)
		}
		if drop == "TABLE" {
			contents.Tables = append(contents.Tables, name)
		} else {
			contents.Others++
		}
	}
	if err := rows.Err(); err != nil {
		return SchemaContents{}, fmt.Errorf("list the schema's contents: %w", err)
	}
	return contents, nil
}

// DropSchemaContents drops everything [Store.SchemaContents] lists, with
// every row in it, in one transaction: afterwards the schema is as empty
// as one skift never migrated, and its own privileges and owner are
// untouched. Waiting for a table's lock gives up after
// [snapshotLockTimeout], so a service still holding one fails the drop
// rather than hanging it.
func (s *Store) DropSchemaContents(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := setLocal(ctx, tx, restoreSessionSettings()); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, schemaObjects)
	if err != nil {
		return fmt.Errorf("list the schema's contents: %w", err)
	}
	drops, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var d [2]string
		err := row.Scan(&d[0], &d[1])
		return d, err
	})
	if err != nil {
		return fmt.Errorf("list the schema's contents: %w", err)
	}
	// Each name is the catalogue's own rendering of an object's name,
	// quoted where SQL needs it. IF EXISTS covers an object an earlier
	// CASCADE already took.
	for _, d := range drops {
		if _, err := tx.Exec(ctx, `DROP `+d[0]+` IF EXISTS `+d[1]+` CASCADE`); err != nil {
			return fmt.Errorf("drop %s %s: %w", strings.ToLower(d[0]), d[1], err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// OtherSessions counts the client sessions connected to this database
// apart from the one asking.
func (s *Store) OtherSessions(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid()
		   AND backend_type = 'client backend'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the database's sessions: %w", err)
	}
	return n, nil
}

// Restore copies a snapshot's rows back into a database whose schema is at
// exactly the snapshot's version, and holds no rows but those its
// migrations wrote: one [Store.Snapshot] took, migrated afresh. It is
// Snapshot in reverse, inside one transaction:
//
//   - it sets snap.Settings, so the COPY text reads back as it was written;
//   - it checks the schema version and every table's columns against snap;
//   - it creates each partition snap lists that the schema lacks;
//   - it empties every table of the rows the migrations wrote, such as
//     goose's own;
//   - for each table, in snap's order, it calls open with the table's name
//     and runs `COPY name (columns) FROM STDIN` over what it returns, then
//     closes it. A partitioned table's rows are routed to its partitions;
//   - it checks that every table holds exactly the rows snap counts;
//   - it sets every sequence to its position in snap.
//
// Only then does it commit, so a restore that fails at any step leaves the
// rows as they were. A mismatch with snap is an [ErrRestoreMismatch].
//
// Foreign keys are not deferred. Snap lists each table after the tables it
// references, and a table that references itself, such as a node's
// parent, is copied by one COPY statement, after which PostgreSQL checks
// constraints that are not deferrable, so its rows may come in any order.
//
// A setting snap names that [Store.Snapshot] does not write under is
// refused, since a session setting such as search_path would change what
// the copy writes to.
func (s *Store) Restore(ctx context.Context, snap Snapshot, open func(table string) (io.ReadCloser, error)) error {
	for _, setting := range snap.Settings {
		if !slices.ContainsFunc(snapshotSettings, func(known [2]string) bool { return strings.EqualFold(known[0], setting[0]) }) {
			return fmt.Errorf("the snapshot sets %s, which a snapshot is never written under", setting[0])
		}
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := setLocal(ctx, tx, append(restoreSessionSettings(), snap.Settings...)); err != nil {
		return err
	}

	version, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version != snap.SchemaVersion {
		return fmt.Errorf("%w: the database is at schema %d, the snapshot at %d", ErrRestoreMismatch, version, snap.SchemaVersion)
	}
	present, err := snapshotTables(ctx, tx)
	if err != nil {
		return err
	}
	if err := sameTables(snap.Tables, present); err != nil {
		return err
	}
	for _, t := range present {
		if err := createPartitions(ctx, tx, t, snapshotTable(snap.Tables, t.Name).Partitions); err != nil {
			return err
		}
	}

	names := make([]string, len(snap.Tables))
	for i, t := range snap.Tables {
		names[i] = pgx.Identifier{t.Name}.Sanitize()
	}
	// No table but goose's holds rows after a fresh migration, and the
	// append-only tables refuse a DELETE only for a row there is to
	// delete. Emptying them in reverse order leaves no row referencing
	// one already gone.
	for i := range slices.Backward(names) {
		if _, err := tx.Exec(ctx, `DELETE FROM `+names[i]); err != nil {
			return fmt.Errorf("empty %s: %w", snap.Tables[i].Name, err)
		}
	}

	for i, t := range snap.Tables {
		if err := copyIn(ctx, tx, names[i], t, open); err != nil {
			return err
		}
	}
	for i, t := range snap.Tables {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+names[i]).Scan(&n); err != nil {
			return fmt.Errorf("count %s: %w", t.Name, err)
		}
		if n != t.Rows {
			return fmt.Errorf("%w: %s holds %d rows after the copy, and the snapshot lists %d", ErrRestoreMismatch, t.Name, n, t.Rows)
		}
	}

	for _, seq := range snap.Sequences {
		_, err := tx.Exec(ctx, `SELECT setval($1::regclass, $2, $3)`, pgx.Identifier{seq.Name}.Sanitize(), seq.LastValue, seq.IsCalled)
		if err != nil {
			return fmt.Errorf("set sequence %s: %w", seq.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// restoreSessionSettings lift the statement timeout, since a large table
// may take long to copy, and bound the wait for a lock.
func restoreSessionSettings() [][2]string {
	return [][2]string{
		{"statement_timeout", "0"},
		{"lock_timeout", fmt.Sprint(snapshotLockTimeout.Milliseconds())},
	}
}

// setLocal sets each [name, value] for the rest of tx.
func setLocal(ctx context.Context, tx pgx.Tx, settings [][2]string) error {
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, setting[0], setting[1]); err != nil {
			return fmt.Errorf("set %s: %w", setting[0], err)
		}
	}
	return nil
}

// sameTables checks that the schema holds exactly the tables the snapshot
// lists, each with the same columns in the same order.
func sameTables(want, present []SnapshotTable) error {
	for _, w := range want {
		p := snapshotTable(present, w.Name)
		if p == nil {
			return fmt.Errorf("%w: the snapshot holds table %s, which the schema does not", ErrRestoreMismatch, w.Name)
		}
		if !slices.Equal(w.Columns, p.Columns) {
			return fmt.Errorf("%w: the snapshot holds %s with columns (%s), and the schema's are (%s)",
				ErrRestoreMismatch, w.Name, strings.Join(w.Columns, ", "), strings.Join(p.Columns, ", "))
		}
	}
	for _, p := range present {
		if snapshotTable(want, p.Name) == nil {
			return fmt.Errorf("%w: the schema has table %s, which the snapshot does not hold", ErrRestoreMismatch, p.Name)
		}
	}
	return nil
}

func snapshotTable(tables []SnapshotTable, name string) *SnapshotTable {
	i := slices.IndexFunc(tables, func(t SnapshotTable) bool { return t.Name == name })
	if i < 0 {
		return nil
	}
	return &tables[i]
}

// rangeBound is a range partition's bound as pg_get_expr prints it: each
// end a list of quoted literals, MINVALUE or MAXVALUE.
var rangeBound = func() *regexp.Regexp {
	value := `(?:'(?:[^']|'')*'|MINVALUE|MAXVALUE)`
	list := `\(` + value + `(?:, ` + value + `)*\)`
	return regexp.MustCompile(`^FOR VALUES FROM ` + list + ` TO ` + list + `$`)
}()

// createPartitions creates each of want that the partitioned table t
// lacks. One it already has must have the same bound. A bound that is not
// a range of literals is refused rather than run as SQL.
func createPartitions(ctx context.Context, tx pgx.Tx, t SnapshotTable, want []SnapshotPartition) error {
	for _, p := range want {
		if i := slices.IndexFunc(t.Partitions, func(have SnapshotPartition) bool { return have.Name == p.Name }); i >= 0 {
			if t.Partitions[i].Bound != p.Bound {
				return fmt.Errorf("%w: partition %s is %s in the schema and %s in the snapshot", ErrRestoreMismatch, p.Name, t.Partitions[i].Bound, p.Bound)
			}
			continue
		}
		if !rangeBound.MatchString(p.Bound) {
			return fmt.Errorf("partition %s has a bound restore does not create: %q", p.Name, p.Bound)
		}
		sql := `CREATE TABLE ` + pgx.Identifier{p.Name}.Sanitize() + ` PARTITION OF ` + pgx.Identifier{t.Name}.Sanitize() + ` ` + p.Bound
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("create partition %s: %w", p.Name, err)
		}
	}
	return nil
}

// copyIn copies one table's rows in from what open returns for it.
func copyIn(ctx context.Context, tx pgx.Tx, name string, t SnapshotTable, open func(table string) (io.ReadCloser, error)) error {
	r, err := open(t.Name)
	if err != nil {
		return err
	}
	columns := make([]string, len(t.Columns))
	for j, c := range t.Columns {
		columns[j] = pgx.Identifier{c}.Sanitize()
	}
	_, err = tx.Conn().PgConn().CopyFrom(ctx, r, `COPY `+name+` (`+strings.Join(columns, ", ")+`) FROM STDIN`)
	if err := errors.Join(err, r.Close()); err != nil {
		return fmt.Errorf("copy %s: %w", t.Name, err)
	}
	return nil
}
