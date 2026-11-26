package store_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Restore", Label(testsupport.DBLabel), func() {
	var (
		source *pgxpool.Pool
		snap   store.Snapshot
		copied map[string][]byte
		target *store.Store
	)

	// fromCopied hands Restore what the snapshot wrote for each table,
	// failing for a table it wrote nothing for.
	fromCopied := func(table string) (io.ReadCloser, error) {
		rows, ok := copied[table]
		if !ok {
			return nil, fmt.Errorf("nothing was copied for %s", table)
		}
		return io.NopCloser(bytes.NewReader(rows)), nil
	}

	BeforeEach(func(ctx SpecContext) {
		source = pg.Database(ctx)
		s := store.FromPool(source)
		org := seedForSnapshot(ctx, s)

		By("a parent rewritten after its child, so the child's row is copied first")
		_, err := source.Exec(ctx, `UPDATE nodes SET title = 'parent, renamed' WHERE title = 'parent'`)
		Expect(err).NotTo(HaveOccurred())
		By("and an event in a partition the target's migrations never make")
		_, err = source.Exec(ctx, `SELECT ensure_event_partitions('2020-01-15', 0)`)
		Expect(err).NotTo(HaveOccurred())
		insertEvent(ctx, source, org, time.Date(2020, 1, 20, 0, 0, 0, 0, time.UTC))

		copied = map[string][]byte{}
		snap, err = s.Snapshot(ctx, func(table string) (io.Writer, error) {
			return writerInto(copied, table), nil
		})
		Expect(err).NotTo(HaveOccurred())
		child := bytes.Index(copied["nodes"], []byte("\tchild\t"))
		parent := bytes.Index(copied["nodes"], []byte("\tparent, renamed\t"))
		Expect(child).To(BeNumerically(">=", 0))
		Expect(parent).To(BeNumerically(">", child), "the spec needs a child copied before its parent")

		target = migratedEmpty(ctx)
	})

	It("copies every table back exactly, at the snapshot's schema, with its sequences where they were", func(ctx SpecContext) {
		Expect(target.Restore(ctx, snap, fromCopied)).To(Succeed())

		want := testsupport.TableRows(ctx, source)
		got := testsupport.TableRows(ctx, target.Pool())
		for table, rows := range want {
			Expect(got[table]).To(Equal(rows), table)
		}
		Expect(got).To(HaveKey("events_202001"), "the partition the snapshot lists is created")
		Expect(want["goose_db_version"]).NotTo(BeEmpty())
		for table, rows := range got {
			if _, ok := want[table]; !ok {
				Expect(rows).To(BeEmpty(), "%s is a partition the target's migrations made, and holds nothing", table)
			}
		}

		version, err := target.SchemaVersion(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(Equal(snap.SchemaVersion))

		for _, seq := range snap.Sequences {
			var last int64
			var called bool
			Expect(target.Pool().QueryRow(ctx, `SELECT last_value, is_called FROM `+seq.Name).Scan(&last, &called)).To(Succeed())
			Expect([]any{last, called}).To(Equal([]any{seq.LastValue, seq.IsCalled}), seq.Name)
		}
		var next, highest int64
		Expect(target.Pool().QueryRow(ctx, `SELECT nextval('river_job_id_seq'), (SELECT max(id) FROM river_job)`).Scan(&next, &highest)).To(Succeed())
		Expect(next).To(BeNumerically(">", highest))
	})

	It("rolls every row back when a table's count does not match the snapshot's", func(ctx SpecContext) {
		before := testsupport.TableRows(ctx, target.Pool())
		i := slices.IndexFunc(snap.Tables, func(t store.SnapshotTable) bool { return t.Name == "nodes" })
		snap.Tables[i].Rows++

		err := target.Restore(ctx, snap, fromCopied)
		Expect(err).To(MatchError(store.ErrRestoreMismatch))
		Expect(err).To(MatchError(ContainSubstring("nodes holds 3 rows after the copy, and the snapshot lists 4")))
		Expect(testsupport.TableRows(ctx, target.Pool())).To(Equal(before))
	})

	It("ends with the error opening a table's rows met, having copied nothing", func(ctx SpecContext) {
		before := testsupport.TableRows(ctx, target.Pool())
		delete(copied, "nodes")
		Expect(target.Restore(ctx, snap, fromCopied)).To(MatchError(ContainSubstring("nothing was copied for nodes")))
		Expect(testsupport.TableRows(ctx, target.Pool())).To(Equal(before))
	})

	It("refuses a database at another schema version, or with other tables, before copying anything", func(ctx SpecContext) {
		before := testsupport.TableRows(ctx, target.Pool())

		older := snap
		older.SchemaVersion--
		Expect(target.Restore(ctx, older, fromCopied)).To(MatchError(ContainSubstring("the database is at schema %d, the snapshot at %d", snap.SchemaVersion, older.SchemaVersion)))

		missing := snap
		missing.Tables = slices.DeleteFunc(slices.Clone(snap.Tables), func(t store.SnapshotTable) bool { return t.Name == "comments" })
		Expect(target.Restore(ctx, missing, fromCopied)).To(MatchError(ContainSubstring("the schema has table comments, which the snapshot does not hold")))

		renamed := snap
		renamed.Tables = slices.Clone(snap.Tables)
		i := slices.IndexFunc(renamed.Tables, func(t store.SnapshotTable) bool { return t.Name == "nodes" })
		renamed.Tables[i].Columns = append(slices.Clone(renamed.Tables[i].Columns), "invented")
		Expect(target.Restore(ctx, renamed, fromCopied)).To(MatchError(store.ErrRestoreMismatch))

		Expect(testsupport.TableRows(ctx, target.Pool())).To(Equal(before))
	})

	It("refuses a partition bound that is not a range of literals, and a setting no snapshot is written under", func(ctx SpecContext) {
		before := testsupport.TableRows(ctx, target.Pool())

		injected := snap
		injected.Tables = slices.Clone(snap.Tables)
		i := slices.IndexFunc(injected.Tables, func(t store.SnapshotTable) bool { return t.Name == "events" })
		injected.Tables[i].Partitions = []store.SnapshotPartition{{
			Name:  "events_190001",
			Bound: `FOR VALUES FROM ('1900-01-01') TO ('1900-02-01'); DROP TABLE comments; --`,
		}}
		Expect(target.Restore(ctx, injected, fromCopied)).To(MatchError(ContainSubstring("partition events_190001 has a bound restore does not create")))

		redirected := snap
		redirected.Settings = append(slices.Clone(snap.Settings), [2]string{"search_path", "elsewhere"})
		Expect(target.Restore(ctx, redirected, fromCopied)).To(MatchError(ContainSubstring("the snapshot sets search_path")))

		Expect(testsupport.TableRows(ctx, target.Pool())).To(Equal(before))
	})
})

var _ = Describe("The schema's contents", Label(testsupport.DBLabel), func() {
	It("are nothing on a database skift never migrated", func(ctx SpecContext) {
		empty, err := store.Open(ctx, pg.EmptyDatabase(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(empty.Close)
		contents, err := empty.SchemaContents(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(contents.Empty()).To(BeTrue())
	})

	It("are every table a migration makes, and dropping them leaves a schema the migrations make again", func(ctx SpecContext) {
		db := pg.Database(ctx)
		s := store.FromPool(db)
		seedForSnapshot(ctx, s)

		contents, err := s.SchemaContents(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(contents.Tables).To(ConsistOf(topLevelTables(ctx, db)))
		Expect(contents.Others).To(BeNumerically(">", 0), "functions, the view, River's enum")

		Expect(s.DropSchemaContents(ctx)).To(Succeed())
		contents, err = s.SchemaContents(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(contents.Empty()).To(BeTrue(), "%+v", contents)
		var left int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace = current_schema()::regnamespace`).Scan(&left)).To(Succeed())
		Expect(left).To(BeZero(), "no relation, partition or index is left")

		Expect(migrations.Migrate(ctx, db.Config().ConnString())).To(Succeed())
	})

	It("counts the other sessions connected to the database", func(ctx SpecContext) {
		db := pg.Database(ctx)
		s := store.FromPool(db)
		alone, err := s.OtherSessions(ctx)
		Expect(err).NotTo(HaveOccurred())

		other, err := pgxpool.New(ctx, db.Config().ConnString())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(other.Close)
		Expect(other.Ping(ctx)).To(Succeed())

		together, err := s.OtherSessions(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(together).To(Equal(alone + 1))
	})
})

// migratedEmpty is a store on a database of its own, migrated to the
// current schema and holding no rows but goose's.
func migratedEmpty(ctx context.Context) *store.Store {
	GinkgoHelper()
	url := pg.EmptyDatabase(ctx)
	Expect(migrations.Migrate(ctx, url)).To(Succeed())
	s, err := store.Open(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(s.Close)
	return s
}

// writerInto is a buffer for table in copied, empty until written to.
func writerInto(copied map[string][]byte, table string) io.Writer {
	copied[table] = []byte{}
	return &appender{copied: copied, table: table}
}

type appender struct {
	copied map[string][]byte
	table  string
}

func (a *appender) Write(p []byte) (int, error) {
	a.copied[a.table] = append(a.copied[a.table], p...)
	return len(p), nil
}
