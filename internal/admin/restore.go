package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/store/migrations"
)

// ErrSchemaNotCarried is what [Restore] refuses a snapshot with when its
// schema version is not one of the migrations this build carries, so the
// build cannot rebuild the schema the rows were copied from.
var ErrSchemaNotCarried = errors.New("this build does not carry the snapshot's schema")

// ErrDatabaseNotEmpty is what [Restore] refuses a database with when its
// schema already holds something and the caller did not ask to clean it.
var ErrDatabaseNotEmpty = errors.New("the database already holds skift's schema")

// ErrSnapshotDamaged is what [Restore] refuses a snapshot file with when
// it is not one [TakeSnapshot] could have written whole.
var ErrSnapshotDamaged = errors.New("the snapshot is damaged")

// RestoreParams is what one [Restore] does.
type RestoreParams struct {
	// Path is the snapshot file to restore.
	Path string
	// Clean drops everything in the database's schema first. Without it,
	// a database whose schema holds anything is refused.
	Clean bool
}

// RestoreResult is what one [Restore] did.
type RestoreResult struct {
	// Path is the snapshot file restored.
	Path string `json:"path"`
	// SchemaVersion is the schema the database is at afterwards: the
	// snapshot's.
	SchemaVersion int64 `json:"schema_version"`
	// TakenAt is when the snapshot was taken.
	TakenAt time.Time `json:"taken_at"`
	// TakenBy is the version of the skift that took it.
	TakenBy string `json:"taken_by"`
	// Cleaned is whether what the database held before was dropped.
	Cleaned bool `json:"cleaned"`
	// Tables is every table restored, with its rows, in the order copied.
	Tables []RestoredTable `json:"tables"`
	// Sequences is how many sequences were set.
	Sequences int `json:"sequences"`
}

// RestoredTable is one table a [Restore] copied back.
type RestoredTable struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// Restore brings the database at databaseURL back to exactly the snapshot
// at params.Path, which [TakeSnapshot] wrote. Only an operator runs it: it
// discards whatever the database holds. See
// docs/adrs/018-unattended-upgrades.md.
//
// Before it changes anything it reads the whole snapshot, refusing one
// whose entries do not match its manifest; refuses a snapshot whose schema
// version this build does not carry; and refuses a database whose schema
// holds anything, unless params.Clean, when it drops all of it. It warns
// on log, without refusing, when other sessions are connected, since the
// service should be stopped first.
//
// It then migrates the empty database to exactly the snapshot's schema
// version and copies the rows back with [store.Store.Restore], in one
// transaction that checks every table's row count before committing. If
// either step fails, it drops what it migrated, so the database is left
// empty; with params.Clean, what it held before is not brought back.
func Restore(ctx context.Context, databaseURL string, params RestoreParams, log *slog.Logger) (RestoreResult, error) {
	archive, err := zip.OpenReader(params.Path)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = archive.Close() }()
	manifest, err := readManifest(&archive.Reader)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := checkSnapshot(&archive.Reader, manifest); err != nil {
		return RestoreResult{}, fmt.Errorf("%w: %s: %w", ErrSnapshotDamaged, params.Path, err)
	}

	db, err := store.Open(ctx, databaseURL)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return RestoreResult{}, fmt.Errorf("connect to database: %w: %w", ErrDatabaseUnreachable, err)
	}
	// Counted before the migrator opens a connection of its own.
	others, err := db.OtherSessions(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	if others > 0 {
		log.Warn("other sessions are connected to the database; stop skift serve before restoring, or what it writes is lost", "sessions", others)
	}

	m, err := migrations.Open(ctx, databaseURL)
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = m.Close() }()
	if !slices.Contains(m.Versions(), manifest.SchemaVersion) {
		return RestoreResult{}, fmt.Errorf("%w: the snapshot is at schema %d, and this build carries schemas up to %d; restore it with a skift whose migrations reach %d",
			ErrSchemaNotCarried, manifest.SchemaVersion, m.Latest(), manifest.SchemaVersion)
	}

	contents, err := db.SchemaContents(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	if !contents.Empty() {
		if !params.Clean {
			version, err := db.SchemaVersion(ctx)
			if err != nil {
				return RestoreResult{}, err
			}
			return RestoreResult{}, fmt.Errorf("%w: it holds %d tables at schema %d; pass --clean to drop them and everything else in the schema first",
				ErrDatabaseNotEmpty, len(contents.Tables), version)
		}
		if err := db.DropSchemaContents(ctx); err != nil {
			return RestoreResult{}, fmt.Errorf("clean the database: %w", err)
		}
		log.Info("dropped what the database held", "tables", len(contents.Tables))
	}

	if err := restoreInto(ctx, db, m, &archive.Reader, manifest); err != nil {
		if derr := db.DropSchemaContents(context.WithoutCancel(ctx)); derr != nil {
			return RestoreResult{}, errors.Join(err, fmt.Errorf("and dropping what the restore migrated: %w", derr))
		}
		return RestoreResult{}, err
	}

	result := RestoreResult{
		Path:          params.Path,
		SchemaVersion: manifest.SchemaVersion,
		TakenAt:       manifest.TakenAt,
		TakenBy:       manifest.SkiftVersion,
		Cleaned:       !contents.Empty(),
		Tables:        make([]RestoredTable, len(manifest.Tables)),
		Sequences:     len(manifest.Sequences),
	}
	for i, t := range manifest.Tables {
		result.Tables[i] = RestoredTable{Name: t.Name, Rows: t.Rows}
	}
	return result, nil
}

// restoreInto migrates an empty database to the manifest's schema and
// copies the archive's rows into it.
func restoreInto(ctx context.Context, db *store.Store, m *migrations.Migrator, archive *zip.Reader, manifest SnapshotManifest) error {
	if err := m.UpTo(ctx, manifest.SchemaVersion); err != nil {
		return fmt.Errorf("migrate to schema %d: %w", manifest.SchemaVersion, err)
	}
	snap := store.Snapshot{
		SchemaVersion: manifest.SchemaVersion,
		Settings:      manifest.Settings,
		Tables:        make([]store.SnapshotTable, len(manifest.Tables)),
		Sequences:     make([]store.SnapshotSequence, len(manifest.Sequences)),
	}
	entries := map[string]string{}
	for i, t := range manifest.Tables {
		snap.Tables[i] = store.SnapshotTable{Name: t.Name, Columns: t.Columns, Rows: t.Rows, Copy: t.Copy}
		for _, p := range t.Partitions {
			snap.Tables[i].Partitions = append(snap.Tables[i].Partitions, store.SnapshotPartition(p))
		}
		entries[t.Name] = t.Entry
	}
	for i, s := range manifest.Sequences {
		snap.Sequences[i] = store.SnapshotSequence(s)
	}
	err := db.Restore(ctx, snap, func(table string) (io.ReadCloser, error) {
		f, err := archive.Open(entries[table])
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", entries[table], err)
		}
		return f, nil
	})
	if err != nil {
		return fmt.Errorf("copy the rows back: %w", err)
	}
	return nil
}

// checkSnapshot reads every table's entry through, so that the archive's
// checksums are verified, and checks it against the manifest: the format
// is one this build reads, each table is listed once with columns and a
// way it was copied, and each entry holds as many lines as the table has
// rows, the last one ended. COPY text escapes a newline within a value,
// so a line is a row.
func checkSnapshot(archive *zip.Reader, m SnapshotManifest) error {
	if m.Format != SnapshotFormat {
		return fmt.Errorf("its format is %d, and this build reads format %d", m.Format, SnapshotFormat)
	}
	if m.SchemaVersion <= 0 || len(m.Tables) == 0 {
		return fmt.Errorf("its manifest names no schema version or no tables")
	}
	seen := map[string]bool{}
	for _, t := range m.Tables {
		switch {
		case t.Name == "" || seen[t.Name]:
			return fmt.Errorf("its manifest lists table %q more than once, or without a name", t.Name)
		case len(t.Columns) == 0:
			return fmt.Errorf("its manifest lists no columns for %s", t.Name)
		case t.Copy != store.CopyTable && t.Copy != store.CopyQuery:
			return fmt.Errorf("its manifest says %s was copied as %q", t.Name, t.Copy)
		}
		seen[t.Name] = true
		lines, err := countLines(archive, t.Entry)
		if err != nil {
			return err
		}
		if lines != t.Rows {
			return fmt.Errorf("%s holds %d rows, and its manifest lists %d", t.Entry, lines, t.Rows)
		}
	}
	return nil
}

// countLines counts the lines of one entry, reading it to the end.
func countLines(archive *zip.Reader, entry string) (int64, error) {
	if !strings.HasPrefix(entry, tableEntryDir) {
		return 0, fmt.Errorf("its manifest names %q, which is not a table's entry", entry)
	}
	f, err := archive.Open(entry)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", entry, err)
	}
	defer func() { _ = f.Close() }()
	var (
		lines int64
		last  byte = '\n'
		buf        = make([]byte, 1<<16)
	)
	for {
		n, err := f.Read(buf)
		lines += int64(bytes.Count(buf[:n], []byte{'\n'}))
		if n > 0 {
			last = buf[n-1]
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", entry, err)
		}
	}
	if last != '\n' {
		return 0, fmt.Errorf("%s ends partway through a row", entry)
	}
	return lines, nil
}
