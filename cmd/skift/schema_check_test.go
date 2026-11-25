package main_test

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/testsupport"
)

// schemaSQL runs one statement against the database at url, for a spec
// that moves the recorded schema version.
func schemaSQL(ctx context.Context, url, sql string, args ...any) {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	_, err = conn.Exec(ctx, sql, args...)
	Expect(err).NotTo(HaveOccurred())
}

// rowCount is how many rows table holds.
func rowCount(ctx context.Context, url, table string) int {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	var n int
	Expect(conn.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n)).To(Succeed())
	return n
}

// latestSchema is the newest migration this build carries.
func latestSchema(ctx context.Context, url string) int64 {
	GinkgoHelper()
	status, err := admin.InspectSchema(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	return status.Latest
}

// writes is what a one-shot command could have written: organizations,
// principals, and queued jobs.
func writes(ctx context.Context, url string) [3]int {
	GinkgoHelper()
	return [3]int{rowCount(ctx, url, "organizations"), rowCount(ctx, url, "principals"), rowCount(ctx, url, "river_job")}
}

var _ = Describe("One-shot admin commands check the schema before acting", Label(testsupport.DBLabel), func() {
	commands := []TableEntry{
		Entry("skift admin bootstrap", "admin", "bootstrap", "1000", "acme"),
		Entry("skift admin jobs list", "admin", "jobs", "list"),
		Entry("skift admin jobs run", "admin", "jobs", "run", "expire-leases"),
	}

	DescribeTable("refuse a database migrated by a newer skift, naming both versions, and write nothing",
		func(ctx SpecContext, args ...string) {
			By("given a database whose schema is one migration ahead of this build")
			url := pg.Database(ctx).Config().ConnString()
			latest := latestSchema(ctx, url)
			schemaSQL(ctx, url, `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, latest+1)
			before := writes(ctx, url)

			By("when the command runs")
			session := skift(binary, append(args, "--database-url", url)...)

			By("then it fails, naming both versions and the way out")
			Expect(session.ExitCode()).To(Equal(cli.ExitFailure), "stdout: %s", session.Out.Contents())
			Expect(string(session.Err.Contents())).To(SatisfyAll(
				ContainSubstring(fmt.Sprintf("schema %d is newer than this build's %d", latest+1, latest)),
				ContainSubstring("restore the pre-upgrade backup or install a newer skift"),
			))
			Expect(session.Out.Contents()).To(BeEmpty())

			By("and nothing was written")
			Expect(writes(ctx, url)).To(Equal(before))
		},
		commands,
	)

	DescribeTable("refuse a database not yet migrated to this build's schema, saying to migrate, and write nothing",
		func(ctx SpecContext, args ...string) {
			By("given a database whose newest migration is not recorded as applied")
			url := pg.Database(ctx).Config().ConnString()
			latest := latestSchema(ctx, url)
			schemaSQL(ctx, url, `DELETE FROM goose_db_version WHERE version_id = $1`, latest)
			before := writes(ctx, url)

			By("when the command runs")
			session := skift(binary, append(args, "--database-url", url)...)

			By("then it fails, naming both versions and the command that catches up")
			Expect(session.ExitCode()).To(Equal(cli.ExitFailure), "stdout: %s", session.Out.Contents())
			Expect(string(session.Err.Contents())).To(SatisfyAll(
				ContainSubstring(fmt.Sprintf("schema %d is behind this build's %d", latest-1, latest)),
				ContainSubstring("run skift admin migrate"),
			))
			Expect(session.Out.Contents()).To(BeEmpty())

			By("and nothing was written")
			Expect(writes(ctx, url)).To(Equal(before))
		},
		commands,
	)
})
