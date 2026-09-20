package migrations_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/store/migrations"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("Applying migrations from nothing", Label(testsupport.DBLabel), func() {
	It("applies every migration to an empty database, then none to a current one", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)

		By("given an empty database, the schema is behind")
		status, err := migrations.Inspect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.Current).To(BeZero())
		Expect(status.Pending()).To(BeTrue())

		By("when the migrations are applied, every version is reported, oldest first")
		m, err := migrations.Open(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		versions := m.Versions()
		Expect(m.Close()).To(Succeed())
		report, err := migrations.Apply(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Applied).To(Equal(versions))
		Expect(report.Version).To(Equal(versions[len(versions)-1]))

		By("then the schema is current")
		status, err = migrations.Inspect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.Pending()).To(BeFalse())
		Expect(status.Current).To(Equal(status.Latest))

		By("and applying again reports nothing applied and is not an error")
		report, err = migrations.Apply(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Applied).To(BeEmpty())
		Expect(report.Version).To(Equal(status.Latest))
	})

	It("tells a database that does not answer apart from one that refuses", func(ctx SpecContext) {
		_, err := migrations.Apply(ctx, "postgres://dagger:dagger@127.0.0.1:1/dagger?sslmode=disable&connect_timeout=1")
		Expect(err).To(MatchError(migrations.ErrUnreachable))
		_, err = migrations.Inspect(ctx, "postgres://dagger:dagger@127.0.0.1:1/dagger?sslmode=disable&connect_timeout=1")
		Expect(err).To(MatchError(migrations.ErrUnreachable))
	})
})
