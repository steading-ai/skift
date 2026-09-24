package main_test

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/ivy/dagger/internal/cli"
	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/testsupport"
)

// principalRows is the count of principals and identities, read straight
// from the database.
func principalRows(ctx context.Context, url string) (principals, identities int) {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	Expect(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM principals), (SELECT count(*) FROM principal_identities)`).
		Scan(&principals, &identities)).To(Succeed())
	return principals, identities
}

func bootstrapped(session *gexec.Session) domain.Bootstrapped {
	GinkgoHelper()
	var out domain.Bootstrapped
	Expect(json.Unmarshal(session.Out.Contents(), &out)).To(Succeed(), "stdout: %s", session.Out.Contents())
	return out
}

var _ = Describe("dg admin bootstrap", func() {
	It("Scenario: Bootstrap is safe against an already-seeded database", Label(testsupport.DBLabel), func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()

		By("given a database that has already been bootstrapped")
		session := dg(binary, "admin", "bootstrap", "--database-url", url, "1000", "acme")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		first := bootstrapped(session)
		Expect(first.Organization.Created).To(BeTrue())
		Expect(first.Principal.Created).To(BeTrue())
		Expect(first.Principal.Kind).To(Equal("user"))
		Expect(first.Principal.Identity).To(Equal(domain.BootstrappedIdentity{Scheme: "unix", Value: "1000"}))
		principals, identities := principalRows(ctx, url)
		Expect(principals).To(Equal(1))
		Expect(identities).To(Equal(1))

		By("when dg admin bootstrap is run again with the same user id")
		session = dg(binary, "admin", "bootstrap", "--database-url", url, "1000", "acme")

		By("then it exits zero and the principal count is unchanged")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		second := bootstrapped(session)
		Expect(second.Organization.ID).To(Equal(first.Organization.ID))
		Expect(second.Principal.ID).To(Equal(first.Principal.ID))
		Expect(second.Organization.Created).To(BeFalse())
		Expect(second.Principal.Created).To(BeFalse())
		principals, identities = principalRows(ctx, url)
		Expect(principals).To(Equal(1))
		Expect(identities).To(Equal(1))
	})

	It("refuses a uid held by a principal of another organization", Label(testsupport.DBLabel), func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()
		Expect(dg(binary, "admin", "bootstrap", "--database-url", url, "1000", "acme").ExitCode()).To(BeZero())
		session := dg(binary, "admin", "bootstrap", "--database-url", url, "1000", "other")
		Expect(session.ExitCode()).To(Equal(cli.ExitFailure))
		Expect(string(session.Err.Contents())).To(ContainSubstring("another organization"))
	})

	It("rejects a uid that is not a number as a usage error", Label("unit"), func() {
		session := dg(binary, "admin", "bootstrap", "--database-url", unreachableDB, "ivy", "acme")
		Expect(session.ExitCode()).To(Equal(cli.ExitUsage))
		Expect(string(session.Err.Contents())).To(ContainSubstring("not a numeric user id"))
	})
})
