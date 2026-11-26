package testsupport

import (
	"context"
	"fmt"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/pressly/goose/v3"

	"github.com/steading-ai/skift/internal/store/migrations"
)

// A NewerMigration is one a newer skift would carry and this build does
// not. [MigratePast] applies it after this build's newest migration, so a
// spec can put a database where an upgrade, and then a downgrade back to
// this build, would leave it. These migrations are never rolled back.
type NewerMigration struct {
	// Name is the file's name after its version.
	Name string
	// Up is the migration's SQL, given the version it is applied as.
	Up func(version int64) string
}

// AddsColumn only adds: a column with a default on every table, which this
// build's queries never name. It leaves the floor alone.
var AddsColumn = NewerMigration{
	Name: "adds_column",
	Up: func(int64) string {
		return `-- +goose StatementBegin
DO $$
DECLARE
    t regclass;
BEGIN
    FOR t IN
        SELECT c.oid::regclass FROM pg_class c
         WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p')
           AND NOT c.relispartition AND c.relname <> 'goose_db_version'
    LOOP
        EXECUTE format('ALTER TABLE %s ADD COLUMN newer_note text NOT NULL DEFAULT %L', t, '');
    END LOOP;
END
$$;
-- +goose StatementEnd`
	},
}

// DropsColumn removes a column this build reads and writes, nodes.body,
// and leaves the floor where it was: the mistake the floor exists to
// catch, since every release at or above the floor still serves the
// schema and then fails on the missing column.
var DropsColumn = NewerMigration{
	Name: "drops_column",
	Up: func(int64) string {
		return `ALTER TABLE nodes DROP COLUMN body;`
	},
}

// RaisesFloor stands for a migration that removes or changes something
// this build relies on. It is reduced to the one statement such a
// migration carries: raising the floor, here to its own version.
var RaisesFloor = NewerMigration{
	Name: "raises_floor",
	Up: func(version int64) string {
		return fmt.Sprintf(`UPDATE schema_floor SET version = %d;`, version)
	},
}

// MigratePast applies newer to the database at url, numbered on from this
// build's newest migration in the order given, as a newer skift would
// apply its own. The database must be at this build's schema. It returns
// this build's newest migration.
func MigratePast(ctx context.Context, url string, newer ...NewerMigration) (latest int64) {
	ginkgo.GinkgoHelper()
	status, err := migrations.Inspect(ctx, url)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(status.Current).To(gomega.Equal(status.Latest), "MigratePast starts from this build's schema")

	files := fstest.MapFS{}
	for i, n := range newer {
		version := status.Latest + int64(i) + 1
		body := "-- +goose Up\n" + n.Up(version) + "\n"
		files[fmt.Sprintf("%05d_%s.sql", version, n.Name)] = &fstest.MapFile{Data: []byte(body)}
	}

	cfg, err := pgx.ParseConfig(url)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	db := stdlib.OpenDB(*cfg)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithLogger(goose.NopLogger()))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer provider.Close() //nolint:errcheck // spec connection
	results, err := provider.Up(ctx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(results).To(gomega.HaveLen(len(newer)))
	return status.Latest
}
