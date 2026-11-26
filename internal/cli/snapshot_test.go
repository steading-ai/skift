package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/testsupport"
	"github.com/steading-ai/skift/internal/ulid"
)

var _ = Describe("skift admin snapshot", func() {
	// run runs skift with args and returns its exit code, stdout and
	// stderr, as a script calling the binary sees them.
	run := func(args ...string) runResult {
		var stdout, stderr bytes.Buffer
		code := cli.Run(args, strings.NewReader(""), &stdout, &stderr)
		return runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}

	Describe("Scenarios", func() {
		It("A snapshot copies every table consistently", Label(testsupport.DBLabel), func(ctx SpecContext) {
			By("given a seeded database at the current schema")
			db := pg.Database(ctx)
			url := db.Config().ConnString()
			out, err := admin.Bootstrap(ctx, testsupport.Store(db), admin.BootstrapParams{OrgName: "acme", UID: 1000})
			Expect(err).NotTo(HaveOccurred())
			for range 2 {
				insertEvent(ctx, db, out.Organization.ID, out.Principal.ID)
			}
			before := tableRowCounts(ctx, db)
			status, err := admin.InspectSchema(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Pending()).To(BeFalse())

			By("when skift admin snapshot runs while another session inserts events")
			holder, err := db.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) { _ = holder.Rollback(ctx) })
			_, err = holder.Exec(ctx, `LOCK TABLE river_queue IN ACCESS EXCLUSIVE MODE`)
			Expect(err).NotTo(HaveOccurred())

			dir := GinkgoT().TempDir()
			done := make(chan runResult, 1)
			go func() {
				defer GinkgoRecover()
				done <- run("admin", "snapshot", "--database-url", url, "--dir", dir)
			}()
			Eventually(func() int { return sessionsWaitingOnLocks(ctx, db) }).WithContext(ctx).Should(Equal(1),
				"the snapshot has begun its transaction and waits to lock the table held")
			for range 3 {
				insertEvent(ctx, db, out.Organization.ID, out.Principal.ID)
			}
			Expect(holder.Rollback(ctx)).To(Succeed())

			var r runResult
			Eventually(done).WithContext(ctx).Should(Receive(&r))
			Expect(r.code).To(BeZero(), r.stderr)
			path := strings.TrimSpace(r.stdout)
			Expect(path).To(BeAnExistingFile())
			Expect(filepath.Dir(path)).To(Equal(dir))

			By("then the snapshot's manifest names the schema version")
			m, err := admin.ReadSnapshotManifest(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.SchemaVersion).To(Equal(status.Current))

			By("and each table's row count matches the moment the copy began")
			names := make([]string, len(m.Tables))
			for i, t := range m.Tables {
				names[i] = t.Name
				Expect(t.Rows).To(Equal(before[t.Name]), t.Name)
			}
			Expect(names).To(ConsistOf(mapKeys(before)))
			Expect(before["events"]).To(Equal(int64(2)))
			Expect(tableRowCounts(ctx, db)["events"]).To(Equal(int64(5)), "the events written meanwhile were committed")
		})

		It("Nothing to migrate means no snapshot", Label(testsupport.DBLabel), func(ctx SpecContext) {
			By("given a database already at the newest schema version")
			db := pg.Database(ctx)
			url := db.Config().ConnString()
			status, err := admin.InspectSchema(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Current).To(BeNumerically(">", 0))

			By("when skift admin snapshot --if-behind that version runs")
			dir := filepath.Join(GinkgoT().TempDir(), "backups")
			r := run("admin", "snapshot", "--database-url", url, "--dir", dir,
				"--if-behind", strconv.FormatInt(status.Current, 10))

			By("then no file is written")
			Expect(dir).NotTo(BeAnExistingFile())
			Expect(r.stdout).To(BeEmpty())
			Expect(r.stderr).To(ContainSubstring("no snapshot taken"))

			By("and it exits 0")
			Expect(r.code).To(BeZero())
		})

		It("A failed snapshot leaves nothing behind", Label("unit"), func() {
			By("given the database cannot be reached")
			dir := GinkgoT().TempDir()
			earlier := filepath.Join(dir, "skift-snapshot-20260101T000000.000Z-schema27.zip")
			Expect(os.WriteFile(earlier, []byte("earlier"), 0o600)).To(Succeed())

			By("when skift admin snapshot runs")
			r := run("admin", "snapshot", "--database-url", unreachableDB, "--dir", dir, "--keep", "1")

			By("then it exits non-zero, saying why on one line of stderr")
			Expect(r.code).NotTo(BeZero())
			Expect(r.stdout).To(BeEmpty())
			Expect(r.stderr).To(ContainSubstring("skift admin snapshot: connect to database: database unreachable"))
			Expect(strings.Count(strings.TrimSpace(r.stderr), "\n")).To(BeZero())

			By("and the backups directory holds no new file")
			entries, err := os.ReadDir(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Name()).To(Equal(filepath.Base(earlier)))
		})
	})

	It("refuses to keep fewer than one snapshot, or to compare against a negative version", Label("unit"), func() {
		r := run("admin", "snapshot", "--database-url", unreachableDB, "--keep", "0")
		Expect(r.code).To(Equal(cli.ExitUsage))
		Expect(r.stderr).To(ContainSubstring("--keep must be at least 1"))
		r = run("admin", "snapshot", "--database-url", unreachableDB, "--if-behind", "-1")
		Expect(r.code).To(Equal(cli.ExitUsage))
		Expect(r.stderr).To(ContainSubstring("--if-behind must be a schema version"))
	})

	It("requires a database URL", Label("unit"), func() {
		GinkgoT().Setenv("DATABASE_URL", "")
		r := run("admin", "snapshot", "--dir", GinkgoT().TempDir())
		Expect(r.code).NotTo(BeZero())
		Expect(r.stderr).To(ContainSubstring("database URL is required"))
	})
})

// runResult is how one run of skift ended, as a script calling it sees.
type runResult struct {
	code           int
	stdout, stderr string
}

// insertEvent records an event in org by principal, now, and commits it.
func insertEvent(ctx context.Context, db *pgxpool.Pool, org, principal string) {
	GinkgoHelper()
	_, err := db.Exec(ctx, `
		INSERT INTO events (id, org_id, principal_id, principal_kind, identity_scheme, policy_version, op, occurred_at)
		VALUES ($1, $2, $3, 'user', 'unix', 'sha256:spec', 'spec', $4)`,
		ulid.New(), org, principal, time.Now())
	Expect(err).NotTo(HaveOccurred())
}

// tableRowCounts counts the rows of every table in the schema that is not
// a partition of another.
func tableRowCounts(ctx context.Context, db *pgxpool.Pool) map[string]int64 {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT tablename::text FROM pg_tables
		 WHERE schemaname = current_schema()
		   AND tablename NOT IN (SELECT inhrelid::regclass::text FROM pg_inherits)`)
	Expect(err).NotTo(HaveOccurred())
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	counts := map[string]int64{}
	for _, table := range tables {
		var n int64
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n)).To(Succeed())
		counts[table] = n
	}
	return counts
}

// sessionsWaitingOnLocks counts the sessions in this database waiting for
// a lock.
func sessionsWaitingOnLocks(ctx context.Context, db *pgxpool.Pool) int {
	GinkgoHelper()
	var n int
	Expect(db.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)).To(Succeed())
	return n
}

func mapKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
