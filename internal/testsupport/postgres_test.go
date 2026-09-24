package testsupport_test

import (
	"context"
	"net/url"
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

	It("Suites share one server through EnvServer, each in a template of its own that ends with it", func(ctx SpecContext) {
		By("given the server this suite runs on, named as a shared one")
		handoff, err := url.Parse(pg.URL())
		Expect(err).NotTo(HaveOccurred())
		own := handoff.Fragment
		handoff.Fragment = ""
		GinkgoT().Setenv(testsupport.EnvServer, handoff.String())

		By("when a second suite starts, it prepares a template of its own rather than a container")
		second, err := testsupport.Start(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		other, err := url.Parse(second.URL())
		Expect(err).NotTo(HaveOccurred())
		Expect(other.Fragment).NotTo(BeEmpty())
		Expect(other.Fragment).NotTo(Equal(own))

		By("and its other processes attach to that template, not this suite's")
		attached, err := testsupport.Attach(ctx, second.URL())
		Expect(err).NotTo(HaveOccurred())
		db := attached.Database(ctx)
		var n int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "cloned from the second suite's unmigrated template")

		By("then stopping the second suite drops its template and nothing else")
		Expect(second.Stop(ctx)).To(Succeed())
		Expect(attached.Stop(ctx)).To(Succeed(), "a process that only attached has nothing to drop")
		conn, err := pgx.Connect(ctx, handoff.String())
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close(ctx) //nolint:errcheck // single-statement session
		Expect(conn.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, other.Fragment).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
		Expect(conn.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, own).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))

		By("and a handoff that names no template is refused")
		_, err = testsupport.Attach(ctx, handoff.String())
		Expect(err).To(MatchError(ContainSubstring("names no template")))
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
