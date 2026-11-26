package admin_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
	"github.com/steading-ai/skift/internal/version"
)

var _ = Describe("Taking a snapshot", Label(testsupport.DBLabel), func() {
	var (
		db  *pgxpool.Pool
		url string
		dir string
	)

	BeforeEach(func(ctx SpecContext) {
		db = pg.Database(ctx)
		url = db.Config().ConnString()
		dir = filepath.Join(GinkgoT().TempDir(), "backups")
		_, err := admin.Bootstrap(ctx, store.FromPool(db), admin.BootstrapParams{OrgName: "acme", UID: 1000})
		Expect(err).NotTo(HaveOccurred())
	})

	take := func(ctx context.Context, params admin.SnapshotParams) (admin.SnapshotResult, error) {
		if params.Dir == "" {
			params.Dir = dir
		}
		if params.Keep == 0 {
			params.Keep = admin.DefaultSnapshotKeep
		}
		return admin.TakeSnapshot(ctx, url, params)
	}

	It("writes one file, private to its owner, holding the manifest and every table's rows", func(ctx SpecContext) {
		out, err := take(ctx, admin.SnapshotParams{})
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Dir(out.Path)).To(Equal(dir))
		Expect(filesIn(dir)).To(ConsistOf(filepath.Base(out.Path)), "no temporary file is left beside it")
		info, err := os.Stat(out.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		dirInfo, err := os.Stat(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(dirInfo.Mode().Perm()).To(Equal(os.FileMode(0o700)))

		m, err := admin.ReadSnapshotManifest(out.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Format).To(Equal(admin.SnapshotFormat))
		Expect(m.SkiftVersion).To(Equal(version.String()))
		status, err := admin.InspectSchema(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.SchemaVersion).To(Equal(status.Current))
		Expect(out.SchemaVersion).To(Equal(status.Current))
		Expect(m.TakenAt).To(BeTemporally("~", time.Now(), time.Minute))
		Expect(filepath.Base(out.Path)).To(MatchRegexp(`^skift-snapshot-\d{8}T\d{6}\.\d{3}Z-schema%d\.zip$`, m.SchemaVersion))

		entries := zipEntries(out.Path)
		Expect(entries).To(HaveKey("manifest.json"))
		Expect(entries).To(HaveLen(len(m.Tables) + 1))
		for _, t := range m.Tables {
			Expect(entries).To(HaveKey(t.Entry), t.Name)
			Expect(int64(bytes.Count(entries[t.Entry], []byte("\n")))).To(Equal(t.Rows), t.Name)
		}
		Expect(tableNames(m)).To(ContainElements("organizations", "principals", "events", "river_job", "goose_db_version"))
		Expect(rowsOf(m, "organizations")).To(Equal(int64(1)))
	})

	Describe("--if-behind", func() {
		It("takes no snapshot, and touches nothing, when the database is at the version or past it", func(ctx SpecContext) {
			status, err := admin.InspectSchema(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			for _, v := range []int64{status.Current, status.Current - 1} {
				began := time.Now()
				out, err := take(ctx, admin.SnapshotParams{IfBehind: v})
				Expect(err).NotTo(HaveOccurred())
				Expect(time.Since(began)).To(BeNumerically("<", time.Second), "an upgrade pays for this on every install")
				Expect(out.Path).To(BeEmpty())
				Expect(out.Skipped).To(ContainSubstring("not behind"))
				Expect(out.SchemaVersion).To(Equal(status.Current))
				Expect(dir).NotTo(BeAnExistingFile(), "not even the directory is created")
			}
		})

		It("takes no snapshot of a database skift has never migrated, which has nothing to lose", func(ctx SpecContext) {
			empty := pg.EmptyDatabase(ctx)
			out, err := admin.TakeSnapshot(ctx, empty, admin.SnapshotParams{Dir: dir, Keep: 1, IfBehind: 30})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Path).To(BeEmpty())
			Expect(out.Skipped).To(ContainSubstring("no skift schema"))
			Expect(out.SchemaVersion).To(BeZero())
			Expect(dir).NotTo(BeAnExistingFile())

			By("and without --if-behind it still fails")
			_, err = admin.TakeSnapshot(ctx, empty, admin.SnapshotParams{Dir: dir, Keep: 1})
			Expect(err).To(MatchError(store.ErrNoSchema))
			Expect(filesIn(dir)).To(BeEmpty())
		})

		It("takes one when the database is behind the version", func(ctx SpecContext) {
			status, err := admin.InspectSchema(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			out, err := take(ctx, admin.SnapshotParams{IfBehind: status.Current + 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Skipped).To(BeEmpty())
			Expect(out.Path).To(BeAnExistingFile())
		})
	})

	Describe("--keep", func() {
		It("leaves exactly the newest snapshots, and nothing else in the directory is touched", func(ctx SpecContext) {
			Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
			earlier := []string{
				"skift-snapshot-20260101T000000.000Z-schema27.zip",
				"skift-snapshot-20260201T000000.000Z-schema27.zip",
				"skift-snapshot-20260301T000000.000Z-schema28.zip",
			}
			for _, name := range append([]string{"notes.txt"}, earlier...) {
				Expect(os.WriteFile(filepath.Join(dir, name), []byte("earlier"), 0o600)).To(Succeed())
			}

			out, err := take(ctx, admin.SnapshotParams{Keep: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(filesIn(dir)).To(ConsistOf(filepath.Base(out.Path), earlier[2], "notes.txt"))
			Expect(out.Removed).To(ConsistOf(filepath.Join(dir, earlier[0]), filepath.Join(dir, earlier[1])))
		})

		It("is at least one", func(ctx SpecContext) {
			_, err := take(ctx, admin.SnapshotParams{Keep: -1})
			Expect(err).To(MatchError(ContainSubstring("keep must be at least 1")))
		})
	})

	Describe("A failed snapshot", func() {
		// earlierSnapshot puts a snapshot in dir, so a spec can see that a
		// failure removes nothing.
		earlierSnapshot := func() string {
			GinkgoHelper()
			Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
			name := "skift-snapshot-20260101T000000.000Z-schema27.zip"
			Expect(os.WriteFile(filepath.Join(dir, name), []byte("earlier"), 0o600)).To(Succeed())
			return name
		}

		It("leaves nothing new when the database cannot be reached", func(ctx SpecContext) {
			name := earlierSnapshot()
			_, err := admin.TakeSnapshot(ctx, unreachableDB, admin.SnapshotParams{Dir: dir, Keep: 1})
			Expect(err).To(MatchError(admin.ErrDatabaseUnreachable))
			Expect(filesIn(dir)).To(ConsistOf(name))
		})

		It("leaves nothing new when the directory cannot be written", func(ctx SpecContext) {
			if os.Geteuid() == 0 {
				Skip("root writes to a directory whatever its mode")
			}
			name := earlierSnapshot()
			Expect(os.Chmod(dir, 0o500)).To(Succeed())
			DeferCleanup(os.Chmod, dir, os.FileMode(0o700))

			_, err := take(ctx, admin.SnapshotParams{Keep: 1})
			Expect(err).To(MatchError(ContainSubstring("create snapshot file")))
			Expect(filesIn(dir)).To(ConsistOf(name))
		})

		It("removes its partial file when the copy fails part way", func(ctx SpecContext) {
			name := earlierSnapshot()

			By("given a table the snapshot will wait on")
			holder, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) { _ = holder.Rollback(ctx) })
			_, err = holder.Exec(ctx, `LOCK TABLE river_queue IN ACCESS EXCLUSIVE MODE`)
			Expect(err).NotTo(HaveOccurred())

			By("when the snapshot is cancelled while it waits")
			snapCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				_, err := take(snapCtx, admin.SnapshotParams{Keep: 1})
				done <- err
			}()
			Eventually(func(g Gomega) {
				g.Expect(filesIn(dir)).To(HaveLen(2), "its temporary file is there while it copies")
				g.Expect(waitingOnLocks(ctx, db)).To(Equal(1))
			}).WithContext(ctx).Should(Succeed())
			cancel()

			By("then it fails and leaves only what was there before")
			Eventually(done).WithContext(ctx).Should(Receive(HaveOccurred()))
			Expect(filesIn(dir)).To(ConsistOf(name))
		})
	})

	Describe("the default directory", func() {
		It("is backups under systemd's state directory, or under /var/lib/skift", func() {
			GinkgoT().Setenv("STATE_DIRECTORY", "/var/lib/skift-test:/var/lib/other")
			Expect(admin.DefaultSnapshotDir()).To(Equal("/var/lib/skift-test/backups"))
			GinkgoT().Setenv("STATE_DIRECTORY", "")
			Expect(admin.DefaultSnapshotDir()).To(Equal("/var/lib/skift/backups"))
		})
	})
})

// unreachableDB is a database URL nothing answers on.
const unreachableDB = "postgres://skift:skift@127.0.0.1:1/skift?sslmode=disable&connect_timeout=1"

// filesIn names every entry in dir; none when it does not exist.
func filesIn(dir string) []string {
	GinkgoHelper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// zipEntries reads every entry of the archive at path.
func zipEntries(path string) map[string][]byte {
	GinkgoHelper()
	archive, err := zip.OpenReader(path)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = archive.Close() }()
	entries := map[string][]byte{}
	for _, f := range archive.File {
		Expect(f.Method).To(Equal(zip.Deflate), f.Name)
		r, err := f.Open()
		Expect(err).NotTo(HaveOccurred())
		entries[f.Name], err = io.ReadAll(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Close()).To(Succeed())
	}
	return entries
}

func tableNames(m admin.SnapshotManifest) []string {
	names := make([]string, len(m.Tables))
	for i, t := range m.Tables {
		names[i] = t.Name
	}
	return names
}

func rowsOf(m admin.SnapshotManifest, table string) int64 {
	for _, t := range m.Tables {
		if t.Name == table {
			return t.Rows
		}
	}
	return -1
}

// waitingOnLocks counts the sessions in this database waiting for a lock.
func waitingOnLocks(ctx context.Context, db *pgxpool.Pool) int {
	GinkgoHelper()
	var n int
	Expect(db.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)).To(Succeed())
	return n
}
