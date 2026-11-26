package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("skift admin restore", func() {
	run := func(args ...string) runResult {
		var stdout, stderr bytes.Buffer
		code := cli.Run(args, strings.NewReader(""), &stdout, &stderr)
		return runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}

	// seededSnapshot seeds a database, snapshots it with the command, and
	// returns the database and the snapshot's path.
	seededSnapshot := func(ctx SpecContext) (*pgxpool.Pool, string) {
		GinkgoHelper()
		db := pg.Database(ctx)
		out, err := admin.Bootstrap(ctx, testsupport.Store(db), admin.BootstrapParams{OrgName: "acme", UID: 1000})
		Expect(err).NotTo(HaveOccurred())
		for range 3 {
			insertEvent(ctx, db, out.Organization.ID, out.Principal.ID)
		}
		r := run("admin", "snapshot", "--database-url", db.Config().ConnString(), "--dir", GinkgoT().TempDir())
		Expect(r.code).To(BeZero(), r.stderr)
		return db, strings.TrimSpace(r.stdout)
	}

	Describe("Scenarios", Label(testsupport.DBLabel), func() {
		It("Restore returns the data exactly", func(ctx SpecContext) {
			By("given a snapshot of a seeded database")
			source, path := seededSnapshot(ctx)
			m, err := admin.ReadSnapshotManifest(path)
			Expect(err).NotTo(HaveOccurred())

			By("when skift admin restore runs against an empty database")
			url := pg.EmptyDatabase(ctx)
			r := run("admin", "restore", path, "--database-url", url)
			Expect(r.code).To(BeZero(), r.stderr)
			var out admin.RestoreResult
			Expect(json.Unmarshal([]byte(r.stdout), &out)).To(Succeed(), "it prints what it restored as JSON")
			Expect(out.Tables).To(ContainElement(admin.RestoredTable{Name: "events", Rows: 3}))

			By("then every table holds exactly the snapshot's rows")
			restored, err := pgxpool.New(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(restored.Close)
			want := testsupport.TableRows(ctx, source)
			Expect(want["events"]).To(HaveLen(3))
			Expect(testsupport.TableRows(ctx, restored)).To(Equal(want))
			Expect(tableRowCounts(ctx, restored)).To(Equal(manifestRows(m)))

			By("and the schema version is the snapshot's")
			status, err := admin.InspectSchema(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Current).To(Equal(m.SchemaVersion))
			Expect(out.SchemaVersion).To(Equal(m.SchemaVersion))
		})

		It("Restore refuses to overwrite by accident", func(ctx SpecContext) {
			_, path := seededSnapshot(ctx)

			By("given a database that holds skift's tables")
			target := pg.Database(ctx)
			url := target.Config().ConnString()
			_, err := admin.Bootstrap(ctx, testsupport.Store(target), admin.BootstrapParams{OrgName: "elsewhere", UID: 2000})
			Expect(err).NotTo(HaveOccurred())
			before := testsupport.TableRows(ctx, target)

			By("when skift admin restore runs without --clean")
			r := run("admin", "restore", path, "--database-url", url)

			By("then it exits non-zero")
			Expect(r.code).NotTo(BeZero())
			Expect(r.stdout).To(BeEmpty())
			Expect(r.stderr).To(ContainSubstring("the database already holds skift's schema"))
			Expect(r.stderr).To(ContainSubstring("pass --clean"))

			By("and the database is unchanged")
			Expect(testsupport.TableRows(ctx, target)).To(Equal(before))
		})
	})

	It("takes exactly one snapshot, and requires a database URL", Label("unit"), func() {
		r := run("admin", "restore", "--database-url", unreachableDB)
		Expect(r.code).To(Equal(cli.ExitUsage))
		GinkgoT().Setenv("DATABASE_URL", "")
		r = run("admin", "restore", "snapshot.zip")
		Expect(r.code).NotTo(BeZero())
		Expect(r.stderr).To(ContainSubstring("database URL is required"))
	})
})

// manifestRows maps each table in m to the rows it lists.
func manifestRows(m admin.SnapshotManifest) map[string]int64 {
	rows := map[string]int64{}
	for _, t := range m.Tables {
		rows[t.Name] = t.Rows
	}
	return rows
}
