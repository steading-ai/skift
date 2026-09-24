package domain_test

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/testsupport"
)

// counts is how many organizations, principals, and identities a database
// holds: what an idempotent bootstrap must leave unchanged.
type counts struct{ orgs, principals, identities int }

func countRows(ctx context.Context, db *pgxpool.Pool) counts {
	GinkgoHelper()
	var c counts
	Expect(db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM organizations),
		(SELECT count(*) FROM principals),
		(SELECT count(*) FROM principal_identities)`).Scan(&c.orgs, &c.principals, &c.identities)).To(Succeed())
	return c
}

var _ = Describe("Bootstrap", Label(testsupport.DBLabel), func() {
	const uid = 1000
	params := domain.BootstrapParams{OrgName: "acme", UID: uid}

	It("creates the organization, a user principal, and its unix identity from nothing", func(ctx SpecContext) {
		db := pg.Database(ctx)
		out, err := domain.Bootstrap(ctx, db, params)
		Expect(err).NotTo(HaveOccurred())

		Expect(out.Organization.Created).To(BeTrue())
		Expect(out.Organization.Name).To(Equal("acme"))
		Expect(out.Principal.Created).To(BeTrue())
		Expect(out.Principal.Kind).To(Equal("user"))
		Expect(out.Principal.Identity).To(Equal(domain.BootstrappedIdentity{Scheme: "unix", Value: "1000"}))

		By("and the identity resolves to the principal the way the socket's interceptor does")
		svc := domain.NewService(db, pol)
		p, err := svc.PrincipalByIdentity(ctx, "unix", "1000")
		Expect(err).NotTo(HaveOccurred())
		Expect(p.ID).To(Equal(out.Principal.ID))
		Expect(p.OrgID).To(Equal(out.Organization.ID))
		Expect(countRows(ctx, db)).To(Equal(counts{1, 1, 1}))
	})

	It("Scenario: Bootstrap is safe against an already-seeded database", func(ctx SpecContext) {
		db := pg.Database(ctx)

		By("given a database that has already been bootstrapped")
		first, err := domain.Bootstrap(ctx, db, params)
		Expect(err).NotTo(HaveOccurred())
		before := countRows(ctx, db)

		By("when bootstrap is run again with the same user id")
		second, err := domain.Bootstrap(ctx, db, params)

		By("then it succeeds, reports the same rows as found, and the counts are unchanged")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Organization.ID).To(Equal(first.Organization.ID))
		Expect(second.Principal.ID).To(Equal(first.Principal.ID))
		Expect(second.Organization.Created).To(BeFalse())
		Expect(second.Principal.Created).To(BeFalse())
		Expect(countRows(ctx, db)).To(Equal(before))
	})

	It("adds a second principal to the same organization for a second uid", func(ctx SpecContext) {
		db := pg.Database(ctx)
		first, err := domain.Bootstrap(ctx, db, params)
		Expect(err).NotTo(HaveOccurred())
		second, err := domain.Bootstrap(ctx, db, domain.BootstrapParams{OrgName: "acme", UID: 1001})
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Organization.ID).To(Equal(first.Organization.ID))
		Expect(second.Organization.Created).To(BeFalse())
		Expect(second.Principal.ID).NotTo(Equal(first.Principal.ID))
		Expect(second.Principal.Created).To(BeTrue())
		Expect(countRows(ctx, db)).To(Equal(counts{1, 2, 2}))
	})

	It("refuses to move an identity held in another organization", func(ctx SpecContext) {
		db := pg.Database(ctx)
		_, err := domain.Bootstrap(ctx, db, params)
		Expect(err).NotTo(HaveOccurred())
		before := countRows(ctx, db)
		_, err = domain.Bootstrap(ctx, db, domain.BootstrapParams{OrgName: "other", UID: uid})
		Expect(err).To(MatchError(domain.ErrIdentityElsewhere))
		By("and the transaction rolled back, so no second organization was left behind")
		Expect(countRows(ctx, db)).To(Equal(before))
	})

	It("converges when several bootstraps race", func(ctx SpecContext) {
		db := pg.Database(ctx)
		const runs = 8
		results := make(chan domain.Bootstrapped, runs)
		var wg sync.WaitGroup
		for range runs {
			wg.Go(func() {
				defer GinkgoRecover()
				out, err := domain.Bootstrap(ctx, db, params)
				Expect(err).NotTo(HaveOccurred())
				results <- out
			})
		}
		wg.Wait()
		close(results)
		ids := map[[2]string]bool{}
		for out := range results {
			ids[[2]string{out.Organization.ID, out.Principal.ID}] = true
		}
		Expect(ids).To(HaveLen(1), "every run must report the same organization and principal")
		Expect(countRows(ctx, db)).To(Equal(counts{1, 1, 1}))
	})
})
