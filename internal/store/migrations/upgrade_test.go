package migrations_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// ADR-015 promises that any release upgrades directly from any earlier one.
// These specs hold migrations to it: every released schema, seeded with a
// row in every table, migrates to the current one; and nothing a release
// shipped ever changes.

var _ = Describe("Upgrading from every release", Label(testsupport.DBLabel), func() {
	It("applies from each released schema, with data in every table, to a fresh install's schema", func(ctx SpecContext) {
		releases := releasedMigrations()
		if releases.versions == nil {
			Skip(releases.unreachable)
		}
		fresh := schema(ctx, pg.Database(ctx))

		for _, v := range releases.versions {
			By(fmt.Sprintf("given a database at released schema %d, with a row in every table", v))
			url := pg.EmptyDatabase(ctx)
			m, err := migrations.Open(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)
			Expect(m.UpTo(ctx, v)).To(Succeed())
			db, err := pgxpool.New(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(db.Close)
			seedFor(v)(ctx, db)
			before := rowCounts(ctx, db)
			for table, n := range before {
				Expect(n).To(BeNumerically(">", 0), "the fixture for schema %d leaves %s empty", v, table)
			}

			By(fmt.Sprintf("when it is upgraded from schema %d to the current one", v))
			Expect(m.Up(ctx)).To(Succeed(), "upgrading from released schema %d", v)

			By("then its schema is a fresh install's, and no table lost a row")
			Expect(schema(ctx, db)).To(Equal(fresh), "upgraded from released schema %d", v)
			after := rowCounts(ctx, db)
			for table, n := range before {
				Expect(after[table]).To(BeNumerically(">=", n), "rows in %s after upgrading from schema %d", table, v)
			}

			By("and the tables River runs skift's background jobs on are there")
			Expect(riverTables(ctx, db)).To(ConsistOf(riverTableNames), "upgraded from released schema %d", v)

			By("and the events partitions reach the horizon the service keeps")
			Expect(partitions(ctx, db)).To(ContainElements(horizonPartitions(time.Now())), "upgraded from released schema %d", v)
		}
	})

	It("installs River's tables on an empty database too", func(ctx SpecContext) {
		Expect(riverTables(ctx, pg.Database(ctx))).To(ConsistOf(riverTableNames))
	})
})

// riverTableNames are the tables River's schema leaves once its version
// 008 is applied: the jobs, the elected leader, the queues, and the
// notifications River's own listener reads.
var riverTableNames = []string{"river_job", "river_leader", "river_notification", "river_queue"}

// riverTables names the River tables in the database's schema.
func riverTables(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename LIKE 'river\_%' ORDER BY tablename`)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}

var _ = Describe("Shipped migrations", Label("unit"), func() {
	It("are never edited, and every new one sorts after them", func() {
		releases := releasedMigrations()
		if releases.versions == nil {
			Skip(releases.unreachable)
		}

		By("given the migrations the newest release " + releases.newestTag + " shipped")
		shipped := releases.newestFiles
		Expect(shipped).NotTo(BeEmpty())

		By("then each is byte for byte what that release shipped")
		for _, name := range shipped {
			want := git("show", releases.newestTag+":internal/store/migrations/"+name)
			got, err := os.ReadFile(name)
			Expect(err).NotTo(HaveOccurred(), "%s shipped in %s and is missing", name, releases.newestTag)
			Expect(string(got)).To(Equal(want), "%s differs from what %s shipped; add a new migration instead", name, releases.newestTag)
		}

		By("and every migration it did not ship has a higher version than any it did")
		newest := migrationVersion(shipped[len(shipped)-1])
		local, err := filepath.Glob("*.sql")
		Expect(err).NotTo(HaveOccurred())
		for _, name := range local {
			if slices.Contains(shipped, name) {
				continue
			}
			Expect(migrationVersion(name)).To(BeNumerically(">", newest), "%s is new since %s but does not sort after its %s", name, releases.newestTag, shipped[len(shipped)-1])
		}
	})
})

// releases is what the release tags reachable from this checkout say about
// migrations: the newest migration version each release shipped, distinct
// and ascending, and the newest release's migration files. versions is nil,
// with unreachable saying why, when no release tag can be read.
type releases struct {
	versions    []int64
	newestTag   string
	newestFiles []string
	unreachable string
}

// releasedMigrations reads the release tags, skipping candidates, since a
// candidate is not a release anyone upgrades from.
func releasedMigrations() releases {
	GinkgoHelper()
	out, err := exec.Command("git", "tag", "--list", "v*", "--sort=v:refname").Output()
	if err != nil {
		return releases{unreachable: "no git repository here to read release tags from: " + err.Error()}
	}
	var tags []string
	for _, tag := range strings.Fields(string(out)) {
		if !strings.Contains(tag, "-") {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return releases{unreachable: "no release tag is reachable from this checkout; fetch tags to run it"}
	}
	r := releases{newestTag: tags[len(tags)-1]}
	for _, tag := range tags {
		files := shippedMigrations(tag)
		Expect(files).NotTo(BeEmpty(), "%s shipped no migrations", tag)
		if v := migrationVersion(files[len(files)-1]); !slices.Contains(r.versions, v) {
			r.versions = append(r.versions, v)
		}
		if tag == r.newestTag {
			r.newestFiles = files
		}
	}
	slices.Sort(r.versions)
	return r
}

// shippedMigrations lists the goose migration files a tag carries, oldest
// first.
func shippedMigrations(tag string) []string {
	GinkgoHelper()
	var files []string
	for _, name := range strings.Split(git("ls-tree", "--name-only", tag+":internal/store/migrations"), "\n") {
		if strings.HasSuffix(name, ".sql") {
			files = append(files, name)
		}
	}
	slices.Sort(files)
	return files
}

// migrationVersion is the version goose reads from a migration's file name.
func migrationVersion(name string) int64 {
	GinkgoHelper()
	prefix, _, found := strings.Cut(name, "_")
	Expect(found).To(BeTrue(), "%s is not named NNNNN_name.sql", name)
	v, err := strconv.ParseInt(prefix, 10, 64)
	Expect(err).NotTo(HaveOccurred(), "%s is not named NNNNN_name.sql", name)
	return v
}

// git runs git from the repository's top level and returns its output.
func git(args ...string) string {
	GinkgoHelper()
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	Expect(err).NotTo(HaveOccurred())
	out, err := exec.Command("git", append([]string{"-C", strings.TrimSpace(string(top))}, args...)...).Output()
	Expect(err).NotTo(HaveOccurred(), "git %s", strings.Join(args, " "))
	return string(out)
}

// partitionHorizon is how many monthly events partitions past the current
// one an upgraded database must have: admin.EventPartitionsAhead, which
// migration 00028 extends every database to.
const partitionHorizon = 3

// horizonPartitions names the events partitions from the month holding now
// to [partitionHorizon] months after it.
func horizonPartitions(now time.Time) []string {
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	names := make([]string, 0, partitionHorizon+1)
	for i := 0; i <= partitionHorizon; i++ {
		names = append(names, partitionName(month.AddDate(0, i, 0)))
	}
	return names
}

// seedFor returns the fixture for a released schema: the one written for
// the newest schema version at or below it. A fixture writes only columns
// its own schema has, so it still applies to a later schema that only adds.
// A released schema it no longer fits needs a fixture of its own, keyed by
// its version.
func seedFor(v int64) func(context.Context, *pgxpool.Pool) {
	GinkgoHelper()
	fixtures := map[int64]func(context.Context, *pgxpool.Pool){
		20: seedSchema20,
		28: seedSchema28,
	}
	var best int64
	for at := range fixtures {
		if at <= v && at > best {
			best = at
		}
	}
	Expect(best).NotTo(BeZero(), "no upgrade fixture is written for released schema %d or earlier", v)
	return fixtures[best]
}

// seedSchema20 writes a row into every table of schema 20, the schema
// every release from v0.1.0 to v0.3.0 shipped, with events in two
// partitions.
func seedSchema20(ctx context.Context, db *pgxpool.Pool) {
	GinkgoHelper()
	f := newFixture(ctx, db)
	addIdentity(ctx, db, f.agent, "unix", "1000@workstation")

	By("a renamed project keeps its old slug as an alias")
	_, err := db.Exec(ctx, `UPDATE projects SET slug = 'platform-renamed' WHERE id = $1`, f.project)
	Expect(err).NotTo(HaveOccurred())

	waiting := newNode(ctx, db, f.org, f.project)
	Expect(insertEdge(ctx, db, f.org, f.node, waiting)).To(Succeed())
	lease := claim(ctx, db, f.org, f.node, f.agent)
	f.estimate(ctx, "similar work", 1000, time.Now())
	f.actual(ctx, lease, 1200, time.Now())

	By("events in this month's partition and the next")
	f.event(ctx, "create_node")
	next := time.Now().UTC().AddDate(0, 1, 0)
	_, err = db.Exec(ctx, `
		INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, request_id, op, before, after, occurred_at)
		VALUES ($1, $2, $3, $4, $5, 'agent', 'unix', 'sha256:spec', $6, 'complete_node', '{"status": "open"}', '{"status": "done"}', $7)`,
		newULID(), f.org, f.project, f.node, f.agent, "req-next-month", next)
	Expect(err).NotTo(HaveOccurred())

	comment := f.comment(ctx, "first draft")
	f.edit(ctx, comment, "second draft")
	f.result(ctx, "done")

	digest := requestHash("blob")
	Expect(f.insertBlob(ctx, digest)).To(Succeed())
	artifact := f.artifact(ctx, "report")
	text := "the report"
	Expect(f.insertPart(ctx, artifact, 0, part{kind: "text", text: &text})).To(Succeed())
	Expect(f.insertPart(ctx, artifact, 1, part{kind: "blob", blob: &digest})).To(Succeed())
	Expect(f.insertKey(ctx, "req-1", requestHash("request"), []byte(`{}`))).To(Succeed())

	_, err = db.Exec(ctx, `
		INSERT INTO policy_denials (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, op, reasons, policy_version)
		VALUES ($1, $2, $3, $4, $5, 'agent', 'unix', 'hold', ARRAY['only a user may hold'], 'sha256:spec')`,
		newULID(), f.org, f.project, f.node, f.agent)
	Expect(err).NotTo(HaveOccurred())
	_, err = db.Exec(ctx, `INSERT INTO claim_cursors (org_id, principal_id, filter_hash, project_id) VALUES ($1, $2, $3, $4)`,
		f.org, f.agent, requestHash("filter"), f.project)
	Expect(err).NotTo(HaveOccurred())
}

// seedSchema28 writes a row into every table of schema 28, the schema
// v0.4.0 shipped: schema 20's rows, an event in every partition of the
// horizon its migration 00028 made, and a row in each of River's tables.
func seedSchema28(ctx context.Context, db *pgxpool.Pool) {
	GinkgoHelper()
	seedSchema20(ctx, db)

	By("an event in every partition the horizon made")
	for _, name := range partitions(ctx, db) {
		var n int64
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{name}.Sanitize()).Scan(&n)).To(Succeed())
		if n > 0 {
			continue
		}
		month, err := time.Parse("200601", strings.TrimPrefix(name, "events_"))
		Expect(err).NotTo(HaveOccurred(), "partition %s", name)
		_, err = db.Exec(ctx, `
			INSERT INTO events (id, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, request_id, op, before, after, occurred_at)
			SELECT $1, org_id, project_id, node_id, principal_id, principal_kind, identity_scheme, policy_version, $2, op, before, after, $3
			  FROM events LIMIT 1`,
			newULID(), "req-"+name, month.AddDate(0, 0, 1))
		Expect(err).NotTo(HaveOccurred(), "an event in %s", name)
	}

	By("a job, a leader, a queue and a notification in River's tables")
	for _, stmt := range []string{
		`INSERT INTO river_job (kind, args, max_attempts) VALUES ('ensure_event_partitions', '{}', 1)`,
		`INSERT INTO river_leader (elected_at, expires_at, leader_id, name) VALUES (now(), now() + interval '1 minute', 'spec', 'default')`,
		`INSERT INTO river_queue (name, updated_at) VALUES ('default', now())`,
		`INSERT INTO river_notification (payload, topic) VALUES ('{}', 'river_insert')`,
	} {
		_, err := db.Exec(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), stmt)
	}
}

// rowCounts counts the rows in every table but goose's own, partitions
// included.
func rowCounts(ctx context.Context, db *pgxpool.Pool) map[string]int64 {
	GinkgoHelper()
	counts := map[string]int64{}
	for _, table := range tables(ctx, db) {
		if table == "goose_db_version" {
			continue
		}
		var n int64
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n)).To(Succeed())
		counts[table] = n
	}
	return counts
}
