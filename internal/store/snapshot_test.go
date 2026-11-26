package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Snapshot", Label(testsupport.DBLabel), func() {
	var (
		db     *pgxpool.Pool
		s      *store.Store
		copied map[string]*bytes.Buffer
		order  []string
		org    string
	)

	// collect hands Snapshot a buffer per table and remembers the order
	// it asked for them in. A table asked for twice is an error.
	collect := func(table string) (io.Writer, error) {
		if _, ok := copied[table]; ok {
			return nil, fmt.Errorf("%s opened twice", table)
		}
		order = append(order, table)
		copied[table] = &bytes.Buffer{}
		return copied[table], nil
	}

	BeforeEach(func(ctx SpecContext) {
		db = pg.Database(ctx)
		s = store.FromPool(db)
		copied, order = map[string]*bytes.Buffer{}, nil
		org = seedForSnapshot(ctx, s)
	})

	It("copies every table in the schema, each with its stored columns, River's and goose's included", func(ctx SpecContext) {
		snap, err := s.Snapshot(ctx, collect)
		Expect(err).NotTo(HaveOccurred())

		names := make([]string, len(snap.Tables))
		for i, t := range snap.Tables {
			names[i] = t.Name
			Expect(t.Columns).To(Equal(columnsOf(ctx, db, t.Name)), t.Name)
		}
		Expect(names).To(ConsistOf(topLevelTables(ctx, db)))
		Expect(names).To(ContainElements("goose_db_version", "river_job", "events", "nodes", "organizations"))
		Expect(order).To(Equal(names), "each table is written in the order the snapshot lists it")
	})

	It("labels the copy with the schema version, the time it began and the settings its text was written under", func(ctx SpecContext) {
		before := time.Now()
		snap, err := s.Snapshot(ctx, collect)
		Expect(err).NotTo(HaveOccurred())

		m, err := migrations.Open(ctx, db.Config().ConnString())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(m.Close)
		Expect(snap.SchemaVersion).To(Equal(m.Latest()))
		Expect(snap.TakenAt).To(BeTemporally("~", before, 5*time.Second))
		Expect(snap.ServerVersion).NotTo(BeEmpty())
		Expect(snap.Settings).To(ContainElement([2]string{"TimeZone", "UTC"}))
	})

	It("counts each table's rows as count(*) does, one line of COPY text per row", func(ctx SpecContext) {
		counts := rowCounts(ctx, db)
		snap, err := s.Snapshot(ctx, collect)
		Expect(err).NotTo(HaveOccurred())

		for _, t := range snap.Tables {
			Expect(t.Rows).To(Equal(counts[t.Name]), t.Name)
			Expect(int64(bytes.Count(copied[t.Name].Bytes(), []byte("\n")))).To(Equal(t.Rows), t.Name)
		}
		Expect(counts["nodes"]).To(BeNumerically(">", 1))
		Expect(counts["events"]).To(BeNumerically(">", 1))
		Expect(copied["nodes"].String()).To(ContainSubstring(`a body\twith a tab\nand a newline`), "text is escaped, so a row is one line")
	})

	It("copies a partitioned table through a query, and lists its partitions with their bounds", func(ctx SpecContext) {
		snap, err := s.Snapshot(ctx, collect)
		Expect(err).NotTo(HaveOccurred())

		for _, t := range snap.Tables {
			if t.Name != "events" {
				Expect(t.Copy).To(Equal(store.CopyTable), t.Name)
				Expect(t.Partitions).To(BeEmpty(), t.Name)
				continue
			}
			Expect(t.Copy).To(Equal(store.CopyQuery))
			partitions := eventPartitionNames(ctx, db)
			Expect(t.Partitions).To(HaveLen(len(partitions)))
			for i, p := range t.Partitions {
				Expect(p.Name).To(Equal(partitions[i]))
				Expect(p.Bound).To(HavePrefix("FOR VALUES FROM ("))
			}
		}
	})

	It("lists each table after every table its foreign keys reference", func(ctx SpecContext) {
		snap, err := s.Snapshot(ctx, collect)
		Expect(err).NotTo(HaveOccurred())
		position := map[string]int{}
		for i, t := range snap.Tables {
			position[t.Name] = i
		}
		for _, ref := range foreignKeys(ctx, db) {
			Expect(position[ref[0]]).To(BeNumerically(">", position[ref[1]]), "%s references %s", ref[0], ref[1])
		}
	})

	It("records every sequence at or ahead of the rows that drew from it", func(ctx SpecContext) {
		snap, err := s.Snapshot(ctx, collect)
		Expect(err).NotTo(HaveOccurred())
		var job *store.SnapshotSequence
		for i := range snap.Sequences {
			if snap.Sequences[i].Name == "river_job_id_seq" {
				job = &snap.Sequences[i]
			}
		}
		Expect(job).NotTo(BeNil())
		var maxID int64
		Expect(db.QueryRow(ctx, `SELECT max(id) FROM river_job`).Scan(&maxID)).To(Succeed())
		Expect(job.IsCalled).To(BeTrue())
		Expect(job.LastValue).To(BeNumerically(">=", maxID))
	})

	It("leaves out writes committed while it copies, and still completes", func(ctx SpecContext) {
		before := rowCounts(ctx, db)
		concurrent := func(table string) (io.Writer, error) {
			if len(order) == 0 {
				By("while the first table is being copied, another session inserts events and a node, and commits")
				for range 3 {
					insertEvent(ctx, db, org, time.Now())
				}
				_, err := db.Exec(ctx, `INSERT INTO nodes (id, number, org_id, project_id, title)
					SELECT $1, max(number) + 1, org_id, project_id, 'written meanwhile' FROM nodes GROUP BY org_id, project_id`, newULID())
				Expect(err).NotTo(HaveOccurred())
			}
			return collect(table)
		}

		snap, err := s.Snapshot(ctx, concurrent)
		Expect(err).NotTo(HaveOccurred())

		for _, t := range snap.Tables {
			Expect(t.Rows).To(Equal(before[t.Name]), t.Name)
		}
		after := rowCounts(ctx, db)
		Expect(after["events"]).To(Equal(before["events"] + 3))
		Expect(after["nodes"]).To(Equal(before["nodes"] + 1))
		Expect(copied["nodes"].String()).NotTo(ContainSubstring("written meanwhile"))
	})

	It("ends with the error that writing a table met", func(ctx SpecContext) {
		full := errors.New("no space left on device")
		_, err := s.Snapshot(ctx, func(string) (io.Writer, error) { return failingWriter{full}, nil })
		Expect(err).To(MatchError(full))
		_, err = s.Snapshot(ctx, func(string) (io.Writer, error) { return nil, full })
		Expect(err).To(MatchError(full))
	})

	It("reads the schema version, and refuses to copy a database skift never migrated", func(ctx SpecContext) {
		version, err := s.SchemaVersion(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(BeNumerically(">", 0))

		empty, err := store.Open(ctx, pg.EmptyDatabase(ctx))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(empty.Close)
		version, err = empty.SchemaVersion(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(BeZero())
		_, err = empty.Snapshot(ctx, collect)
		Expect(err).To(MatchError(store.ErrNoSchema))

		By("and reading the version created nothing")
		var tables int
		Expect(empty.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = current_schema()`).Scan(&tables)).To(Succeed())
		Expect(tables).To(BeZero())
	})
})

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// seedForSnapshot writes rows into the graph's tables, events in two
// partitions, and a River job, and returns the organization.
func seedForSnapshot(ctx context.Context, s *store.Store) string {
	GinkgoHelper()
	org := newULID()
	Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: org, Name: "acme"})
		Expect(err).NotTo(HaveOccurred())
		user, err := tx.CreatePrincipal(ctx, store.CreatePrincipalParams{ID: newULID(), OrgID: org, Kind: "user"})
		Expect(err).NotTo(HaveOccurred())
		project, err := tx.CreateProject(ctx, store.CreateProjectParams{ID: newULID(), OrgID: org, Slug: "platform", Title: "Platform", Priority: "p2"})
		Expect(err).NotTo(HaveOccurred())
		parent, err := tx.CreateNode(ctx, store.CreateNodeParams{ID: newULID(), Number: 1, OrgID: org, ProjectID: project.ID, Kind: "task", Title: "parent", Body: "a body\twith a tab\nand a newline", Priority: "p2", Labels: []string{}})
		Expect(err).NotTo(HaveOccurred())
		child, err := tx.CreateNode(ctx, store.CreateNodeParams{ID: newULID(), Number: 2, OrgID: org, ProjectID: project.ID, ParentID: &parent.ID, Kind: "task", Title: "child", Priority: "p2", Labels: []string{}})
		Expect(err).NotTo(HaveOccurred())
		blocked, err := tx.CreateNode(ctx, store.CreateNodeParams{ID: newULID(), Number: 3, OrgID: org, ProjectID: project.ID, Kind: "task", Title: "blocked", Priority: "p2", Labels: []string{}})
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.CreateEdge(ctx, store.CreateEdgeParams{OrgID: org, FromNode: child.ID, ToNode: blocked.ID, Kind: "blocks"})
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.CreateComment(ctx, store.CreateCommentParams{ID: newULID(), OrgID: org, NodeID: child.ID, Author: user.ID, Body: "looks right"})
		Expect(err).NotTo(HaveOccurred())
		_, err = store.Raw(tx).Exec(ctx, `INSERT INTO river_job (kind, args, max_attempts) VALUES ('ensure_event_partitions', '{}', 1)`)
		Expect(err).NotTo(HaveOccurred())
		return nil
	})).To(Succeed())
	insertEvent(ctx, s.Pool(), org, time.Now())
	insertEvent(ctx, s.Pool(), org, time.Now().AddDate(0, 1, 0))
	return org
}

// insertEvent records an event in org as having happened at at, by its
// first principal, and commits it.
func insertEvent(ctx context.Context, db *pgxpool.Pool, org string, at time.Time) {
	GinkgoHelper()
	_, err := db.Exec(ctx, `
		INSERT INTO events (id, org_id, principal_id, principal_kind, identity_scheme, policy_version, op, occurred_at)
		SELECT $1, $2, id, kind, 'unix', 'sha256:spec', 'spec', $3 FROM principals WHERE org_id = $2 LIMIT 1`,
		newULID(), org, at)
	Expect(err).NotTo(HaveOccurred())
}

// topLevelTables names every table in the schema that is not a partition
// of another, from pg_tables rather than the query the snapshot uses.
func topLevelTables(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT tablename::text FROM pg_tables
		 WHERE schemaname = current_schema()
		   AND tablename NOT IN (SELECT inhrelid::regclass::text FROM pg_inherits)`)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}

// rowCounts counts the rows of every top-level table, a partitioned
// table's being those of all its partitions.
func rowCounts(ctx context.Context, db *pgxpool.Pool) map[string]int64 {
	GinkgoHelper()
	counts := map[string]int64{}
	for _, table := range topLevelTables(ctx, db) {
		var n int64
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n)).To(Succeed())
		counts[table] = n
	}
	return counts
}

// columnsOf names a table's columns from information_schema, in order.
func columnsOf(ctx context.Context, db *pgxpool.Pool, table string) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT column_name::text FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = $1 AND is_generated = 'NEVER'
		 ORDER BY ordinal_position`, table)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return names
}

// eventPartitionNames names the partitions of events, in name order.
func eventPartitionNames(ctx context.Context, db *pgxpool.Pool) []string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `SELECT inhrelid::regclass::text FROM pg_inherits WHERE inhparent = 'events'::regclass`)
	Expect(err).NotTo(HaveOccurred())
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	slices.Sort(names)
	return names
}

// foreignKeys lists each foreign key between two different top-level
// tables as [referencing, referenced].
func foreignKeys(ctx context.Context, db *pgxpool.Pool) [][2]string {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT DISTINCT tc.table_name::text, ccu.table_name::text
		  FROM information_schema.table_constraints tc
		  JOIN information_schema.constraint_column_usage ccu
		    ON ccu.constraint_name = tc.constraint_name AND ccu.constraint_schema = tc.constraint_schema
		 WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = current_schema()
		   AND tc.table_name <> ccu.table_name`)
	Expect(err).NotTo(HaveOccurred())
	var refs [][2]string
	for rows.Next() {
		var ref [2]string
		Expect(rows.Scan(&ref[0], &ref[1])).To(Succeed())
		refs = append(refs, ref)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	partitions := eventPartitionNames(ctx, db)
	refs = slices.DeleteFunc(refs, func(ref [2]string) bool {
		return slices.Contains(partitions, ref[0]) || slices.Contains(partitions, ref[1])
	})
	Expect(refs).NotTo(BeEmpty())
	return refs
}
