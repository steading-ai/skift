package migrations_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// The safeguards docs/adrs/015-upgrades-and-migrations.md puts in code:
// one migrating process at a time, and a short lock timeout retried within
// a budget. Which newer schemas a binary refuses is the floor's, in
// floor_test.go.
var _ = Describe("Migration safeguards", Label(testsupport.DBLabel), func() {
	versionsOf := func(ctx context.Context, url string) []int64 {
		GinkgoHelper()
		m, err := migrations.Open(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		defer m.Close() //nolint:errcheck // read-only use
		return m.Versions()
	}

	// appliedCounts is how many times goose recorded each version applied.
	appliedCounts := func(ctx context.Context, url string) map[int64]int {
		GinkgoHelper()
		conn, err := pgx.Connect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close(ctx) //nolint:errcheck // spec connection
		rows, err := conn.Query(ctx, `SELECT version_id, count(*) FROM goose_db_version WHERE is_applied AND version_id > 0 GROUP BY version_id`)
		Expect(err).NotTo(HaveOccurred())
		counts := map[int64]int{}
		for rows.Next() {
			var v int64
			var n int
			Expect(rows.Scan(&v, &n)).To(Succeed())
			counts[v] = n
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return counts
	}

	It("records each version exactly once when two processes migrate an empty database at once", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)
		versions := versionsOf(ctx, url)

		By("when two applies start together against an empty database")
		var wg sync.WaitGroup
		reports := make([]migrations.Report, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Go(func() {
				defer GinkgoRecover()
				reports[i], errs[i] = migrations.Apply(ctx, url)
			})
		}
		wg.Wait()

		By("then both succeed, and between them they applied every version once")
		Expect(errs).To(HaveEach(Succeed()))
		applied := slices.Concat(reports[0].Applied, reports[1].Applied)
		slices.Sort(applied)
		Expect(applied).To(Equal(versions))

		By("and goose recorded every version exactly once")
		counts := appliedCounts(ctx, url)
		Expect(counts).To(HaveLen(len(versions)))
		for v, n := range counts {
			Expect(n).To(Equal(1), "version %d recorded %d times", v, n)
		}
	})

	It("waits for another process's migration lock, applying nothing until it is released", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)

		By("given another session holding skift's migration lock")
		conn, err := pgx.Connect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = conn.Close(context.Background()) })
		_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrations.LockID)
		Expect(err).NotTo(HaveOccurred())

		By("when the migrations are applied")
		done := make(chan error, 1)
		go func() { _, err := migrations.Apply(ctx, url); done <- err }()

		By("then nothing is applied while the lock is held")
		Consistently(done, 3*time.Second).ShouldNot(Receive())
		var tables int
		Expect(conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'organizations'`).Scan(&tables)).To(Succeed())
		Expect(tables).To(BeZero())

		By("and the run completes once the lock is released")
		_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrations.LockID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(done, 30*time.Second).Should(Receive(Succeed()))
	})

	Describe("lock timeouts", func() {
		const altersProjects = int64(18) // 00018 adds a column to projects

		// holdProjects takes a lock on projects that 00018's ALTER TABLE
		// must wait for, and returns the release.
		holdProjects := func(ctx context.Context, url string) (release func()) {
			GinkgoHelper()
			conn, err := pgx.Connect(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			tx, err := conn.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = tx.Exec(ctx, `SELECT 1 FROM projects LIMIT 1`)
			Expect(err).NotTo(HaveOccurred())
			var once sync.Once
			release = func() {
				once.Do(func() {
					_ = tx.Rollback(context.Background())
					_ = conn.Close(context.Background())
				})
			}
			DeferCleanup(release)
			return release
		}

		It("retries a migration that timed out waiting for a lock, and succeeds once the lock is released", func(ctx SpecContext) {
			url := pg.EmptyDatabase(ctx)
			m, err := migrations.Open(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)
			Expect(m.UpTo(ctx, altersProjects-1)).To(Succeed())

			By("given another session holding a lock 00018 must wait for")
			release := holdProjects(ctx, url)

			By("when the migrations are applied, and the lock is released after a few seconds")
			done := make(chan error, 1)
			go func() { _, err := m.Apply(ctx); done <- err }()
			time.Sleep(migrations.LockTimeout + time.Second)
			release()

			By("then the run succeeds within the retry budget")
			Eventually(done, migrations.RetryBudget).Should(Receive(Succeed()))

			By("after at least one statement was cancelled with SQLSTATE 55P03 and retried")
			Expect(m.LockTimeouts()).To(BeNumerically(">=", 1))
			Expect(appliedCounts(ctx, url)).To(HaveKeyWithValue(altersProjects, 1))
		})

		It("gives up with the lock timeout once the retry budget is spent", func(ctx SpecContext) {
			url := pg.EmptyDatabase(ctx)
			m, err := migrations.Open(ctx, url)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(m.Close)
			Expect(m.UpTo(ctx, altersProjects-1)).To(Succeed())
			m.SetRetryBudget(3 * time.Second)

			By("given a lock 00018 must wait for that is never released")
			holdProjects(ctx, url)

			By("when the migrations are applied")
			_, err = m.Apply(ctx)

			By("then the run fails with PostgreSQL's lock_timeout error, and 00018 is not recorded")
			var pgErr *pgconn.PgError
			Expect(errors.As(err, &pgErr)).To(BeTrue(), "error: %v", err)
			Expect(pgErr.Code).To(Equal("55P03"))
			Expect(err.Error()).To(ContainSubstring("gave up waiting for locks"))
			Expect(appliedCounts(ctx, url)).NotTo(HaveKey(altersProjects))
		})
	})
})
