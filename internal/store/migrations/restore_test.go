package migrations_test

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// ADR-018 makes a snapshot the way back from a release that has migrated:
// this build restores one taken at any released schema, and leaves the
// database at that schema, for the release that shipped it to serve.

var _ = Describe("Restoring a snapshot taken at a released schema", Label(testsupport.DBLabel), func() {
	It("leaves the database at that schema, exactly as the release's migrations build it, with every row", func(ctx SpecContext) {
		releases := releasedMigrations()
		if releases.versions == nil {
			Skip(releases.unreachable)
		}
		for _, v := range releases.versions {
			By(fmt.Sprintf("given a snapshot of a database at released schema %d, with a row in every table", v))
			sourceURL := pg.EmptyDatabase(ctx)
			sm, err := migrations.Open(ctx, sourceURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(sm.Close)
			Expect(sm.UpTo(ctx, v)).To(Succeed())
			source, err := pgxpool.New(ctx, sourceURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(source.Close)
			seedFor(v)(ctx, source)
			taken, err := admin.TakeSnapshot(ctx, sourceURL, admin.SnapshotParams{Dir: GinkgoT().TempDir(), Keep: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(taken.SchemaVersion).To(Equal(v))

			By("when this build restores it into an empty database")
			url := pg.EmptyDatabase(ctx)
			out, err := admin.Restore(ctx, url, admin.RestoreParams{Path: taken.Path}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			Expect(err).NotTo(HaveOccurred(), "restoring a snapshot at schema %d", v)
			restored, err := pgxpool.New(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(restored.Close)

			By(fmt.Sprintf("then the database is at schema %d, the newest migration the release that shipped it carries", v))
			Expect(out.SchemaVersion).To(Equal(v))
			status, err := migrations.Inspect(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.Current).To(Equal(v))

			By("and its schema is the one that release's migrations build, which the shipped-migrations spec holds unchanged")
			Expect(schema(ctx, restored)).To(Equal(schema(ctx, source)), "restored at schema %d", v)

			By("and every table holds exactly the rows the snapshot was taken from")
			Expect(testsupport.TableRows(ctx, restored)).To(Equal(testsupport.TableRows(ctx, source)), "restored at schema %d", v)
		}
	})
})
