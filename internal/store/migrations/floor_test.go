package migrations_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// ADR-018's compatibility floor: a release serves any schema whose floor
// its newest migration meets, even one newer than itself, and refuses any
// other. A migration that only adds leaves the floor alone; one that
// removes or changes something raises it.

var _ = Describe("The compatibility floor", func() {
	Describe("Status", Label("unit"), func() {
		DescribeTable("serves a schema whose floor the binary carries, and refuses one above it",
			func(status migrations.Status, servable, current bool) {
				if servable {
					Expect(status.Servable()).To(Succeed())
				} else {
					var floor *migrations.SchemaFloorError
					Expect(errors.As(status.Servable(), &floor)).To(BeTrue())
					Expect(*floor).To(Equal(migrations.SchemaFloorError{Current: status.Current, Floor: status.Floor, Latest: status.Latest}))
				}
				if current {
					Expect(status.Check()).To(Succeed())
				} else {
					Expect(status.Check()).NotTo(Succeed())
				}
			},
			Entry("at this build's schema", migrations.Status{Current: 30, Latest: 30, Floor: 29}, true, true),
			Entry("behind this build", migrations.Status{Current: 29, Latest: 30, Floor: 29}, true, false),
			Entry("ahead of this build, floor below it", migrations.Status{Current: 32, Latest: 30, Floor: 29}, true, true),
			Entry("ahead of this build, floor at it", migrations.Status{Current: 32, Latest: 30, Floor: 30}, true, true),
			Entry("ahead of this build, floor above it", migrations.Status{Current: 32, Latest: 30, Floor: 31}, false, false),
		)

		It("names the floor, this build's newest migration and the way out", func() {
			err := migrations.Status{Current: 32, Latest: 30, Floor: 31}.Servable()
			Expect(err).To(MatchError("schema 32 needs migration 31, and this build's newest is 30: install skift that carries migration 31, or restore a snapshot"))
		})
	})

	Describe("in the database", Label(testsupport.DBLabel), func() {
		It("starts at the migration that introduced it, and a schema from before it has its own version as its floor", func(ctx SpecContext) {
			url := pg.EmptyDatabase(ctx)
			m, err := migrations.Open(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)

			introduced := firstVersionWithFloor(ctx, m, url)

			By("a schema from before the floor has its own version as its floor")
			Expect(m.DownTo(ctx, introduced-1)).To(Succeed())
			status, err := m.Status(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Floor).To(Equal(introduced - 1))

			By("and the migration that introduced the floor seeds it with its own version")
			Expect(m.UpTo(ctx, introduced)).To(Succeed())
			status, err = m.Status(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Floor).To(Equal(introduced))
		})

		It("Scenario: A migration that only adds leaves the floor alone", func(ctx SpecContext) {
			url := pg.Database(ctx).Config().ConnString()

			By("given a database at the floor")
			before, err := migrations.Inspect(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(before.Current).To(Equal(before.Latest))

			By("when an additive migration is applied")
			latest := testsupport.MigratePast(ctx, url, testsupport.AddsColumn)

			By("then the floor is unchanged")
			after, err := migrations.Inspect(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(after.Current).To(Equal(latest + 1))
			Expect(after.Floor).To(Equal(before.Floor))
		})

		It("serves a database migrated past this build within its floor, applying nothing", func(ctx SpecContext) {
			url := pg.Database(ctx).Config().ConnString()
			latest := testsupport.MigratePast(ctx, url, testsupport.AddsColumn)

			By("then inspecting it reports the schema ahead, within its floor")
			status, err := migrations.Inspect(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Ahead()).To(BeTrue())
			Expect(status.Pending()).To(BeFalse())
			Expect(status.Floor).To(BeNumerically("<=", latest))
			Expect(status.Servable()).To(Succeed())
			Expect(status.Check()).To(Succeed())

			By("and applying migrations applies nothing and leaves the schema where it was")
			report, err := migrations.Apply(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Applied).To(BeEmpty())
			Expect(report.Version).To(Equal(latest + 1))
		})

		It("refuses a database whose floor is above this build, applying nothing and naming both", func(ctx SpecContext) {
			url := pg.Database(ctx).Config().ConnString()
			latest := testsupport.MigratePast(ctx, url, testsupport.AddsColumn, testsupport.RaisesFloor)

			By("then inspecting it reports the floor above this build")
			status, err := migrations.Inspect(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Floor).To(Equal(latest + 2))

			By("and applying migrations refuses, naming the floor, this build's newest and the way out")
			report, err := migrations.Apply(ctx, url)
			var floor *migrations.SchemaFloorError
			Expect(errors.As(err, &floor)).To(BeTrue(), "error: %v", err)
			Expect(*floor).To(Equal(migrations.SchemaFloorError{Current: latest + 2, Floor: latest + 2, Latest: latest}))
			Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("install skift that carries migration %d, or restore a snapshot", latest+2)))
			Expect(report.Applied).To(BeEmpty())
		})
	})

	// An older binary within the floor still writes to the newer schema, so
	// a migration that leaves the floor alone may not remove or change what
	// that binary reads and writes. sqlc expands every SELECT * at generate
	// time, so a new column never reaches an older binary's reads; a new NOT
	// NULL column without a default would fail its inserts.
	Describe("A migration that leaves the floor alone", Label(testsupport.DBLabel), func() {
		It("never removes or changes a column, nor adds one an older binary's inserts would miss", func(ctx SpecContext) {
			url := pg.EmptyDatabase(ctx)
			m, err := migrations.Open(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)
			db, err := pgxpool.New(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(db.Close)

			for _, v := range m.Versions() {
				before, floorBefore, hadFloor := columns(ctx, db), floorOf(ctx, db), hasFloor(ctx, db)
				Expect(m.UpTo(ctx, v)).To(Succeed())
				after, floorAfter := columns(ctx, db), floorOf(ctx, db)
				if !hasFloor(ctx, db) {
					continue
				}
				Expect(floorAfter).To(BeNumerically("<=", v), "migration %d raises the floor past itself", v)
				if !hadFloor {
					continue
				}
				Expect(floorAfter).To(BeNumerically(">=", floorBefore), "migration %d lowers the floor", v)
				if floorAfter == floorBefore {
					Expect(breaksOlderBinaries(before, after)).To(BeEmpty(), "migration %d leaves the floor at %d", v, floorAfter)
				}
			}
		})

		It("is caught when it adds a NOT NULL column without a default", func(ctx SpecContext) {
			db := pg.Database(ctx)
			before := columns(ctx, db)
			testsupport.MigratePast(ctx, db.Config().ConnString(), testsupport.NewerMigration{
				Name: "adds_required_column",
				Up:   func(int64) string { return `ALTER TABLE projects ADD COLUMN required_note text NOT NULL;` },
			})
			Expect(breaksOlderBinaries(before, columns(ctx, db))).To(ConsistOf("projects.required_note is added NOT NULL without a default"))
		})

		It("is not caught when it adds a column with a default", func(ctx SpecContext) {
			db := pg.Database(ctx)
			before := columns(ctx, db)
			testsupport.MigratePast(ctx, db.Config().ConnString(), testsupport.AddsColumn)
			Expect(breaksOlderBinaries(before, columns(ctx, db))).To(BeEmpty())
		})
	})
})

// firstVersionWithFloor migrates the empty database at url one version at
// a time until it records a floor, and returns that version.
func firstVersionWithFloor(ctx context.Context, m *migrations.Migrator, url string) int64 {
	GinkgoHelper()
	db, err := pgxpool.New(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer db.Close()
	for _, v := range m.Versions() {
		Expect(m.UpTo(ctx, v)).To(Succeed())
		if hasFloor(ctx, db) {
			return v
		}
	}
	Fail("no migration introduces the floor")
	return 0
}

// hasFloor reports whether the schema records a floor.
func hasFloor(ctx context.Context, db *pgxpool.Pool) bool {
	GinkgoHelper()
	var ok bool
	Expect(db.QueryRow(ctx, `SELECT to_regclass('schema_floor') IS NOT NULL`).Scan(&ok)).To(Succeed())
	return ok
}

// floorOf is the recorded floor, or 0 when the schema records none.
func floorOf(ctx context.Context, db *pgxpool.Pool) int64 {
	GinkgoHelper()
	if !hasFloor(ctx, db) {
		return 0
	}
	var floor int64
	Expect(db.QueryRow(ctx, `SELECT version FROM schema_floor`).Scan(&floor)).To(Succeed())
	return floor
}

// column is what an older binary depends on in one column.
type column struct {
	dataType   string
	nullable   bool
	hasDefault bool
}

// columns maps each table in the public schema, partitions left out, to
// its columns.
func columns(ctx context.Context, db *pgxpool.Pool) map[string]map[string]column {
	GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT c.table_name, c.column_name, c.data_type, c.is_nullable = 'YES', c.column_default IS NOT NULL
		  FROM information_schema.columns c
		  JOIN pg_class t ON t.relname = c.table_name AND t.relnamespace = 'public'::regnamespace
		 WHERE c.table_schema = 'public' AND NOT t.relispartition AND t.relkind IN ('r', 'p')`)
	Expect(err).NotTo(HaveOccurred())
	tables := map[string]map[string]column{}
	var table, name string
	var col column
	_, err = pgx.ForEachRow(rows, []any{&table, &name, &col.dataType, &col.nullable, &col.hasDefault}, func() error {
		if tables[table] == nil {
			tables[table] = map[string]column{}
		}
		tables[table][name] = col
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
	return tables
}

// breaksOlderBinaries names each change between two schemas that a binary
// written against the first could trip over in the second: a table or
// column removed, a column's type changed or made NOT NULL, and a NOT NULL
// column without a default added to a table that already existed.
func breaksOlderBinaries(before, after map[string]map[string]column) []string {
	var found []string
	for table, cols := range before {
		now, ok := after[table]
		if !ok {
			found = append(found, table+" is removed")
			continue
		}
		for name, was := range cols {
			is, ok := now[name]
			switch {
			case !ok:
				found = append(found, table+"."+name+" is removed")
			case is.dataType != was.dataType:
				found = append(found, fmt.Sprintf("%s.%s changes type from %s to %s", table, name, was.dataType, is.dataType))
			case was.nullable && !is.nullable:
				found = append(found, table+"."+name+" is made NOT NULL")
			}
		}
		for name, is := range now {
			if _, existed := cols[name]; !existed && !is.nullable && !is.hasDefault {
				found = append(found, table+"."+name+" is added NOT NULL without a default")
			}
		}
	}
	return found
}
