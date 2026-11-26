package admin_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
	"github.com/steading-ai/skift/internal/ulid"
)

var _ = Describe("Restoring a snapshot", Label(testsupport.DBLabel), func() {
	var (
		source   *pgxpool.Pool
		path     string
		manifest admin.SnapshotManifest
		logged   *bytes.Buffer
		log      *slog.Logger
	)

	BeforeEach(func(ctx SpecContext) {
		source = pg.Database(ctx)
		out, err := admin.Bootstrap(ctx, store.FromPool(source), admin.BootstrapParams{OrgName: "acme", UID: 1000})
		Expect(err).NotTo(HaveOccurred())
		for range 3 {
			_, err := source.Exec(ctx, `
				INSERT INTO events (id, org_id, principal_id, principal_kind, identity_scheme, policy_version, op, occurred_at)
				VALUES ($1, $2, $3, 'user', 'unix', 'sha256:spec', 'spec', now())`,
				ulid.New(), out.Organization.ID, out.Principal.ID)
			Expect(err).NotTo(HaveOccurred())
		}
		taken, err := admin.TakeSnapshot(ctx, source.Config().ConnString(), admin.SnapshotParams{Dir: GinkgoT().TempDir(), Keep: 1})
		Expect(err).NotTo(HaveOccurred())
		path = taken.Path
		manifest, err = admin.ReadSnapshotManifest(path)
		Expect(err).NotTo(HaveOccurred())
		logged = &bytes.Buffer{}
		log = slog.New(slog.NewTextHandler(logged, nil))
	})

	It("migrates an empty database to the snapshot's schema, copies every row back, and says what it restored", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		out, err := admin.Restore(ctx, url, admin.RestoreParams{Path: path}, log)
		Expect(err).NotTo(HaveOccurred())

		Expect(out.Path).To(Equal(path))
		Expect(out.SchemaVersion).To(Equal(manifest.SchemaVersion))
		Expect(out.TakenAt).To(Equal(manifest.TakenAt))
		Expect(out.Cleaned).To(BeFalse())
		Expect(out.Tables).To(HaveLen(len(manifest.Tables)))
		for i, t := range manifest.Tables {
			Expect(out.Tables[i]).To(Equal(admin.RestoredTable{Name: t.Name, Rows: t.Rows}))
		}
		Expect(out.Sequences).To(Equal(len(manifest.Sequences)))

		restored := openPool(ctx, url)
		Expect(testsupport.TableRows(ctx, restored)).To(Equal(testsupport.TableRows(ctx, source)))
		status, err := admin.InspectSchema(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.Current).To(Equal(manifest.SchemaVersion))
		Expect(logged.String()).NotTo(ContainSubstring("other sessions"))
	})

	It("refuses a database that holds skift's schema, and with clean replaces what it held", func(ctx SpecContext) {
		target := pg.Database(ctx)
		url := target.Config().ConnString()
		_, err := admin.Bootstrap(ctx, store.FromPool(target), admin.BootstrapParams{OrgName: "elsewhere", UID: 2000})
		Expect(err).NotTo(HaveOccurred())
		before := testsupport.TableRows(ctx, target)
		target.Close()

		_, err = admin.Restore(ctx, url, admin.RestoreParams{Path: path}, log)
		Expect(err).To(MatchError(admin.ErrDatabaseNotEmpty))
		Expect(err).To(MatchError(ContainSubstring("pass --clean")))
		Expect(testsupport.TableRows(ctx, openPool(ctx, url))).To(Equal(before))

		out, err := admin.Restore(ctx, url, admin.RestoreParams{Path: path, Clean: true}, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Cleaned).To(BeTrue())
		Expect(testsupport.TableRows(ctx, openPool(ctx, url))).To(Equal(testsupport.TableRows(ctx, source)))
	})

	It("refuses a snapshot whose entries do not match its manifest, before touching the database", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		for name, edit := range map[string]func(admin.SnapshotManifest, map[string][]byte){
			"a row count": func(m admin.SnapshotManifest, entries map[string][]byte) {
				entries["tables/events.copy"] = entries["tables/events.copy"][:bytes.IndexByte(entries["tables/events.copy"], '\n')+1]
			},
			"a truncated row": func(m admin.SnapshotManifest, entries map[string][]byte) {
				e := entries["tables/organizations.copy"]
				entries["tables/organizations.copy"] = e[:len(e)-1]
			},
			"a format": func(m admin.SnapshotManifest, entries map[string][]byte) {
				m.Format = admin.SnapshotFormat + 1
				entries["manifest.json"] = marshalManifest(m)
			},
		} {
			damaged := rewriteSnapshot(path, edit)
			_, err := admin.Restore(ctx, url, admin.RestoreParams{Path: damaged}, log)
			Expect(err).To(MatchError(admin.ErrSnapshotDamaged), name)
		}
		Expect(schemaIsEmpty(ctx, url)).To(BeTrue())
	})

	It("refuses a snapshot at a schema this build does not carry, before touching the database", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		newer := rewriteSnapshot(path, func(m admin.SnapshotManifest, entries map[string][]byte) {
			m.SchemaVersion = 9999
			entries["manifest.json"] = marshalManifest(m)
		})
		_, err := admin.Restore(ctx, url, admin.RestoreParams{Path: newer}, log)
		Expect(err).To(MatchError(admin.ErrSchemaNotCarried))
		Expect(err).To(MatchError(ContainSubstring("the snapshot is at schema 9999")))
		Expect(schemaIsEmpty(ctx, url)).To(BeTrue())
	})

	It("rolls the whole restore back when the database refuses a row, leaving the database empty", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		duplicated := rewriteSnapshot(path, func(m admin.SnapshotManifest, entries map[string][]byte) {
			entries["tables/organizations.copy"] = bytes.Repeat(entries["tables/organizations.copy"], 2)
			for i := range m.Tables {
				if m.Tables[i].Name == "organizations" {
					m.Tables[i].Rows *= 2
				}
			}
			entries["manifest.json"] = marshalManifest(m)
		})
		_, err := admin.Restore(ctx, url, admin.RestoreParams{Path: duplicated}, log)
		Expect(err).To(MatchError(ContainSubstring("copy organizations")))
		Expect(schemaIsEmpty(ctx, url)).To(BeTrue(), "what the restore migrated is dropped again")
	})

	It("warns, and goes on, when other sessions are connected", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		other := openPool(ctx, url)
		Expect(other.Ping(ctx)).To(Succeed())

		_, err := admin.Restore(ctx, url, admin.RestoreParams{Path: path}, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(logged.String()).To(ContainSubstring("level=WARN"))
		Expect(logged.String()).To(ContainSubstring("other sessions are connected"))
		Expect(logged.String()).To(ContainSubstring("sessions=1"))
	})

	It("fails, having changed nothing, on a file that is not a snapshot or a database that cannot be reached", func(ctx SpecContext) {
		notSnapshot := filepath.Join(GinkgoT().TempDir(), "notes.zip")
		Expect(os.WriteFile(notSnapshot, []byte("not a zip"), 0o600)).To(Succeed())
		_, err := admin.Restore(ctx, unreachableDB, admin.RestoreParams{Path: notSnapshot}, log)
		Expect(err).To(MatchError(ContainSubstring("open snapshot")))

		_, err = admin.Restore(ctx, unreachableDB, admin.RestoreParams{Path: path}, log)
		Expect(err).To(MatchError(admin.ErrDatabaseUnreachable))
	})
})

// openPool opens a pool on url, closed when the spec ends.
func openPool(ctx context.Context, url string) *pgxpool.Pool {
	GinkgoHelper()
	pool, err := pgxpool.New(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	return pool
}

// schemaIsEmpty reports whether the database at url holds nothing of
// skift's: no table, not even goose's.
func schemaIsEmpty(ctx context.Context, url string) bool {
	GinkgoHelper()
	s, err := store.Open(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer s.Close()
	contents, err := s.SchemaContents(ctx)
	Expect(err).NotTo(HaveOccurred())
	return contents.Empty()
}

// rewriteSnapshot copies the snapshot at path to a new file, letting edit
// change its manifest's entry and its tables' entries first, and returns
// the copy's path. The entries keep their order, the manifest last.
func rewriteSnapshot(path string, edit func(admin.SnapshotManifest, map[string][]byte)) string {
	GinkgoHelper()
	m, err := admin.ReadSnapshotManifest(path)
	Expect(err).NotTo(HaveOccurred())
	entries := zipEntries(path)
	names := make([]string, 0, len(entries))
	archive, err := zip.OpenReader(path)
	Expect(err).NotTo(HaveOccurred())
	for _, f := range archive.File {
		names = append(names, f.Name)
	}
	Expect(archive.Close()).To(Succeed())
	edit(m, entries)

	out := filepath.Join(GinkgoT().TempDir(), filepath.Base(path))
	f, err := os.Create(out)
	Expect(err).NotTo(HaveOccurred())
	w := zip.NewWriter(f)
	for _, name := range names {
		e, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		Expect(err).NotTo(HaveOccurred())
		_, err = io.Copy(e, bytes.NewReader(entries[name]))
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(w.Close()).To(Succeed())
	Expect(f.Close()).To(Succeed())
	return out
}

func marshalManifest(m admin.SnapshotManifest) []byte {
	GinkgoHelper()
	raw, err := json.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	return raw
}
