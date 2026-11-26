package admin

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/version"
)

// DefaultSnapshotKeep is how many snapshots [TakeSnapshot] keeps when the
// caller does not say.
const DefaultSnapshotKeep = 5

// SnapshotFormat is the version of the snapshot file's layout, written in
// its manifest. A change a restore of an earlier layout cannot read raises
// it.
const SnapshotFormat = 1

// Names inside a snapshot file, and of the file itself.
const (
	snapshotPrefix   = "skift-snapshot-"
	snapshotSuffix   = ".zip"
	snapshotTimeForm = "20060102T150405.000Z"
	manifestEntry    = "manifest.json"
	tableEntryDir    = "tables/"
	tableEntrySuffix = ".copy"
)

// DefaultSnapshotDir is where snapshots go unless the caller says: the
// backups directory under systemd's StateDirectory when the service
// manager names one, as it does for skift.service, and under
// /var/lib/skift otherwise.
func DefaultSnapshotDir() string {
	state, _, _ := strings.Cut(os.Getenv("STATE_DIRECTORY"), ":")
	if state == "" {
		state = "/var/lib/skift"
	}
	return filepath.Join(state, "backups")
}

// SnapshotParams is what one [TakeSnapshot] does.
type SnapshotParams struct {
	// Dir is the directory the snapshot is written to, created with mode
	// 0700 when missing.
	Dir string
	// Keep is how many snapshots, this one included, are left in Dir
	// afterwards; older ones are removed. At least 1.
	Keep int
	// IfBehind, when positive, takes a snapshot only if the database's
	// schema version is below it.
	IfBehind int64
}

// SnapshotResult is what one [TakeSnapshot] did.
type SnapshotResult struct {
	// Path is the snapshot file written; empty when none was.
	Path string `json:"path,omitempty"`
	// Skipped says why no snapshot was taken; empty when one was.
	Skipped string `json:"skipped,omitempty"`
	// SchemaVersion is the database's schema version.
	SchemaVersion int64 `json:"schema_version"`
	// Removed lists the older snapshots removed to keep Keep.
	Removed []string `json:"removed,omitempty"`
}

// SnapshotManifest is the manifest.json of a snapshot file. A snapshot is
// one zip archive, named skift-snapshot-<UTC time>-schema<version>.zip,
// such as skift-snapshot-20261125T153000.000Z-schema28.zip, so that names
// sort in the order the snapshots were taken. It holds:
//
//   - manifest.json, this manifest, as JSON.
//   - tables/<name>.copy for each table in Tables: the table's rows in
//     PostgreSQL's COPY text format (tab-separated, \N for NULL, one row
//     per line), encoded UTF-8, written under Settings, with the columns
//     in Columns order. Restoring one is
//     `COPY name (columns) FROM STDIN` in a session with the same
//     Settings, whether Copy is "table" or "query".
//
// Every entry is deflated. The entries are written in Tables order with
// the manifest last, so a reader opens the archive by its central
// directory rather than streaming it.
//
// Tables lists every ordinary and partitioned table in skift's schema,
// River's and goose's among them, each after the tables its foreign keys
// reference; tables that reference each other in a cycle come last, in
// name order. A self-reference, such as a node's parent, is not ordered,
// so restoring that table needs its own rows in the order they were
// copied or its constraint checked at commit.
type SnapshotManifest struct {
	// Format is [SnapshotFormat].
	Format int `json:"format"`
	// SkiftVersion is the version of the skift binary that took it.
	SkiftVersion string `json:"skift_version"`
	// SchemaVersion is the newest migration applied when the copy began.
	// A restore migrates an empty database to exactly this version before
	// copying the rows back.
	SchemaVersion int64 `json:"schema_version"`
	// TakenAt is when the copy's transaction began, in UTC.
	TakenAt time.Time `json:"taken_at"`
	// PostgresVersion is the version of the server it was taken from.
	PostgresVersion string `json:"postgres_version"`
	// Settings are the session settings the COPY text was written under,
	// as [name, value] pairs, to set before reading it back.
	Settings [][2]string `json:"settings"`
	// Tables is every table, in an order a restore can copy them back in.
	Tables []SnapshotTable `json:"tables"`
	// Sequences is every sequence in the schema with its position, to
	// set with setval once the rows are back. A sequence may be ahead of
	// the rows, never behind.
	Sequences []SnapshotSequence `json:"sequences"`
}

// SnapshotTable is one table in a [SnapshotManifest].
type SnapshotTable struct {
	// Name is the table's name, unqualified.
	Name string `json:"name"`
	// Entry is the path in the archive of the table's COPY text.
	Entry string `json:"entry"`
	// Columns names the columns in the order each row holds them.
	Columns []string `json:"columns"`
	// Rows is how many rows the table held when the copy began, and how
	// many lines its entry has.
	Rows int64 `json:"rows"`
	// Copy is "table" for `COPY name (columns) TO STDOUT`, or "query" for
	// `COPY (SELECT columns FROM name) TO STDOUT`, which a partitioned
	// table needs.
	Copy string `json:"copy"`
	// Partitions lists a partitioned table's partitions, to create before
	// copying its rows back.
	Partitions []SnapshotPartition `json:"partitions,omitempty"`
}

// SnapshotPartition is one partition of a table in a [SnapshotManifest].
type SnapshotPartition struct {
	// Name is the partition's table name.
	Name string `json:"name"`
	// Bound is its bound as PostgreSQL prints it, to follow
	// `CREATE TABLE name PARTITION OF parent`.
	Bound string `json:"bound"`
}

// SnapshotSequence is one sequence in a [SnapshotManifest]: restoring it is
// `setval(name, last_value, is_called)`.
type SnapshotSequence struct {
	Name      string `json:"name"`
	LastValue int64  `json:"last_value"`
	IsCalled  bool   `json:"is_called"`
}

// TakeSnapshot writes a snapshot of the database at databaseURL into
// params.Dir and prunes older ones down to params.Keep. See
// [SnapshotManifest] for the file.
//
// It reads the schema but neither checks it against this binary nor needs
// it to be current: a snapshot is of whatever schema the database is at,
// labelled with it. With params.IfBehind it first reads only the schema
// version, and when that is already at or above IfBehind returns with
// Skipped set, having written and removed nothing.
//
// The file is written under a temporary name in Dir, synced, and renamed
// into place only once complete, so a snapshot that fails or is
// interrupted leaves no file under a snapshot's name. A failure removes no
// older snapshot. A failure to remove an older snapshot after a new one is
// in place is not an error: it is left for the next run, and the result
// lists only what was removed.
func TakeSnapshot(ctx context.Context, databaseURL string, params SnapshotParams) (SnapshotResult, error) {
	if params.Keep < 1 {
		return SnapshotResult{}, fmt.Errorf("keep must be at least 1, not %d", params.Keep)
	}
	db, err := store.Open(ctx, databaseURL)
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return SnapshotResult{}, fmt.Errorf("connect to database: %w: %w", ErrDatabaseUnreachable, err)
	}

	if params.IfBehind > 0 {
		current, err := db.SchemaVersion(ctx)
		if err != nil {
			return SnapshotResult{}, err
		}
		if current >= params.IfBehind {
			return SnapshotResult{
				SchemaVersion: current,
				Skipped:       fmt.Sprintf("the database is at schema %d, not behind %d", current, params.IfBehind),
			}, nil
		}
	}

	if err := os.MkdirAll(params.Dir, 0o700); err != nil {
		return SnapshotResult{}, fmt.Errorf("create snapshot directory: %w", err)
	}
	tmp, err := os.CreateTemp(params.Dir, "."+snapshotPrefix+"*.tmp")
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("create snapshot file: %w", err)
	}
	manifest, err := writeSnapshot(ctx, db, tmp)
	if err != nil {
		_ = tmp.Close()
		return SnapshotResult{}, errors.Join(err, removeTemp(tmp.Name()))
	}
	path := filepath.Join(params.Dir, snapshotName(manifest))
	if err := os.Rename(tmp.Name(), path); err != nil {
		return SnapshotResult{}, errors.Join(fmt.Errorf("name snapshot file: %w", err), removeTemp(tmp.Name()))
	}
	if err := syncDir(params.Dir); err != nil {
		return SnapshotResult{}, errors.Join(fmt.Errorf("sync snapshot directory: %w", err), os.Remove(path))
	}

	result := SnapshotResult{Path: path, SchemaVersion: manifest.SchemaVersion}
	result.Removed = pruneSnapshots(params.Dir, filepath.Base(path), params.Keep)
	return result, nil
}

// writeSnapshot copies the database into f as a snapshot archive, then
// syncs and closes f. It returns the manifest it wrote.
func writeSnapshot(ctx context.Context, db *store.Store, f *os.File) (SnapshotManifest, error) {
	archive := zip.NewWriter(f)
	snap, err := db.Snapshot(ctx, func(table string) (io.Writer, error) {
		w, err := archive.CreateHeader(&zip.FileHeader{Name: tableEntry(table), Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return nil, fmt.Errorf("write %s: %w", f.Name(), err)
		}
		return w, nil
	})
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("copy database: %w", err)
	}
	manifest := newManifest(snap)
	w, err := archive.CreateHeader(&zip.FileHeader{Name: manifestEntry, Method: zip.Deflate, Modified: manifest.TakenAt})
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("write %s: %w", f.Name(), err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return SnapshotManifest{}, fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := archive.Close(); err != nil {
		return SnapshotManifest{}, fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return SnapshotManifest{}, fmt.Errorf("sync %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		return SnapshotManifest{}, fmt.Errorf("close %s: %w", f.Name(), err)
	}
	return manifest, nil
}

func newManifest(snap store.Snapshot) SnapshotManifest {
	m := SnapshotManifest{
		Format:          SnapshotFormat,
		SkiftVersion:    version.String(),
		SchemaVersion:   snap.SchemaVersion,
		TakenAt:         snap.TakenAt.UTC(),
		PostgresVersion: snap.ServerVersion,
		Settings:        snap.Settings,
		Tables:          make([]SnapshotTable, len(snap.Tables)),
		Sequences:       make([]SnapshotSequence, len(snap.Sequences)),
	}
	for i, t := range snap.Tables {
		m.Tables[i] = SnapshotTable{Name: t.Name, Entry: tableEntry(t.Name), Columns: t.Columns, Rows: t.Rows, Copy: t.Copy}
		for _, p := range t.Partitions {
			m.Tables[i].Partitions = append(m.Tables[i].Partitions, SnapshotPartition(p))
		}
	}
	for i, s := range snap.Sequences {
		m.Sequences[i] = SnapshotSequence(s)
	}
	return m
}

func tableEntry(table string) string {
	return tableEntryDir + table + tableEntrySuffix
}

// snapshotName is the file name of the snapshot m describes.
func snapshotName(m SnapshotManifest) string {
	return fmt.Sprintf("%s%s-schema%d%s", snapshotPrefix, m.TakenAt.UTC().Format(snapshotTimeForm), m.SchemaVersion, snapshotSuffix)
}

// isSnapshotName reports whether name is a snapshot file's: what pruning
// counts and removes, and nothing else in the directory.
func isSnapshotName(name string) bool {
	return strings.HasPrefix(name, snapshotPrefix) && strings.HasSuffix(name, snapshotSuffix)
}

// pruneSnapshots removes all but the keep newest snapshots in dir, newest
// by name, and returns the paths it removed. The snapshot named latest,
// the one just taken, is always among those kept, even if a clock set
// back has put a later name beside it.
func pruneSnapshots(dir, latest string, keep int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && isSnapshotName(e.Name()) && e.Name() != latest {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	slices.Reverse(names)
	var removed []string
	for _, name := range names[min(keep-1, len(names)):] {
		path := filepath.Join(dir, name)
		if os.Remove(path) == nil {
			removed = append(removed, path)
		}
	}
	return removed
}

// removeTemp removes a temporary snapshot file that will never be renamed.
func removeTemp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// syncDir makes a rename into dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// ReadSnapshotManifest reads the manifest of the snapshot file at path.
func ReadSnapshotManifest(path string) (SnapshotManifest, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = archive.Close() }()
	return readManifest(&archive.Reader)
}

// readManifest reads the manifest of an open snapshot archive.
func readManifest(archive *zip.Reader) (SnapshotManifest, error) {
	f, err := archive.Open(manifestEntry)
	if err != nil {
		return SnapshotManifest{}, fmt.Errorf("open snapshot manifest: %w", err)
	}
	defer func() { _ = f.Close() }()
	var m SnapshotManifest
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return SnapshotManifest{}, fmt.Errorf("read snapshot manifest: %w", err)
	}
	return m, nil
}
