package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/testsupport"
)

// migrateReport mirrors what skift migrate prints.
type migrateReport struct {
	Applied []int64 `json:"applied"`
	Version int64   `json:"version"`
}

// skift runs one invocation of the binary and waits for it to exit.
func skift(binary string, args ...string) *gexec.Session {
	GinkgoHelper()
	session, err := gexec.Start(exec.Command(binary, args...), GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, 60).Should(gexec.Exit(), "skift %v did not exit", args)
	return session
}

// schemaVersion reads the newest applied migration straight from goose's
// table, so a spec checks the database rather than the command's word.
func schemaVersion(ctx context.Context, url string) int64 {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	var v int64
	Expect(conn.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version`).Scan(&v)).To(Succeed())
	return v
}

// markAhead records a migration newer than any this build carries, as a
// newer skift would have, and returns the build's newest version.
func markAhead(ctx context.Context, url string) (latest int64) {
	GinkgoHelper()
	latest = schemaVersion(ctx, url)
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	_, err = conn.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, latest+1)
	Expect(err).NotTo(HaveOccurred())
	return latest
}

// newerSchema is what skift says when the database is ahead of it.
func newerSchema(latest int64) string {
	return fmt.Sprintf("schema %d is newer than this build's %d: restore the pre-upgrade backup or install a newer skift", latest+1, latest)
}

var _ = Describe("skift migrate", func() {
	It("refuses a database migrated by a newer skift, naming both versions", Label(testsupport.DBLabel), func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()
		latest := markAhead(ctx, url)

		By("when skift migrate runs against a database ahead of it")
		session := skift(binary, "migrate", "--database-url", url)

		By("then it exits non-zero, names both versions, and changes nothing")
		Expect(session.ExitCode()).To(Equal(cli.ExitFailure))
		Expect(string(session.Err.Contents())).To(ContainSubstring(newerSchema(latest)))
		Expect(schemaVersion(ctx, url)).To(Equal(latest + 1))
	})

	It("brings an empty database to the current schema, and a second run applies nothing", Label(testsupport.DBLabel), func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)

		By("when skift migrate runs against an empty database")
		session := skift(binary, "migrate", "--database-url", url)

		By("then it exits zero and reports every migration applied")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		var first migrateReport
		Expect(json.Unmarshal(session.Out.Contents(), &first)).To(Succeed(), "stdout: %s", session.Out.Contents())
		Expect(first.Applied).NotTo(BeEmpty())
		Expect(first.Version).To(Equal(first.Applied[len(first.Applied)-1]))
		Expect(schemaVersion(ctx, url)).To(Equal(first.Version))

		By("when skift migrate runs a second time")
		session = skift(binary, "migrate", "--database-url", url)

		By("then it exits zero, applies nothing, and the schema is where it was")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		var second migrateReport
		Expect(json.Unmarshal(session.Out.Contents(), &second)).To(Succeed(), "stdout: %s", session.Out.Contents())
		Expect(second.Applied).To(BeEmpty())
		Expect(second.Version).To(Equal(first.Version))
		Expect(schemaVersion(ctx, url)).To(Equal(first.Version))
	})

	It("fails without a database URL", Label("unit"), func() {
		cmd := exec.Command(binary, "migrate")
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Eventually(session, 30).Should(gexec.Exit(cli.ExitFailure))
		Expect(string(session.Err.Contents())).To(ContainSubstring("DATABASE_URL"))
	})
})
