package testsupport_test

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("PostgreSQL harness", Label(testsupport.DBLabel), func() {
	// The same slug inserted from several specs at once. Each Describe body
	// below is a separate spec, so under -p they land on different
	// processes and share nothing but the container.
	const slug = "platform"

	insertProject := func(ctx context.Context, db *pgxpool.Pool) {
		GinkgoHelper()
		_, err := db.Exec(ctx, `INSERT INTO projects (slug) VALUES ($1)`, slug)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("Specs are isolated from one another", func() {
		It("the first spec inserts a project with the slug", func(ctx SpecContext) {
			insertProject(ctx, pg.Database(ctx))
		})

		It("the second spec inserts a project with the same slug", func(ctx SpecContext) {
			insertProject(ctx, pg.Database(ctx))
		})

		It("a spec sees only its own rows", func(ctx SpecContext) {
			db := pg.Database(ctx)
			var n int
			Expect(db.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero(), "the template must hold no rows and no other spec's database is visible")
			insertProject(ctx, db)
		})
	})

	It("The template is migrated once and every clone carries the schema", func(ctx SpecContext) {
		db := pg.Database(ctx)
		var exists bool
		Expect(db.QueryRow(ctx, `SELECT to_regclass('projects') IS NOT NULL`).Scan(&exists)).To(Succeed())
		Expect(exists).To(BeTrue())
	})

	It("Databases cloned concurrently from the template do not interfere", func(ctx SpecContext) {
		const clones = 8
		var wg sync.WaitGroup
		errs := make(chan error, clones)
		for range clones {
			wg.Go(func() {
				defer GinkgoRecover()
				db := pg.Database(ctx)
				_, err := db.Exec(ctx, `INSERT INTO projects (slug) VALUES ($1)`, slug)
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("A spec database is dropped when the spec ends", func(ctx SpecContext) {
		var name string
		// Cleanups run in reverse order of registration, so one registered
		// before Database() observes the server after the harness dropped
		// the spec's database.
		DeferCleanup(func(ctx context.Context) {
			admin, err := pgx.Connect(ctx, pg.URL())
			Expect(err).NotTo(HaveOccurred())
			defer admin.Close(ctx) //nolint:errcheck // single-statement session
			var exists bool
			Expect(admin.QueryRow(ctx, `SELECT count(*) > 0 FROM pg_database WHERE datname = $1`, name).Scan(&exists)).To(Succeed())
			Expect(exists).To(BeFalse(), "database %s should have been dropped", name)
		})

		db := pg.Database(ctx)
		Expect(db.QueryRow(ctx, `SELECT current_database()`).Scan(&name)).To(Succeed())
		Expect(name).To(HavePrefix("spec_"))
	})
})
