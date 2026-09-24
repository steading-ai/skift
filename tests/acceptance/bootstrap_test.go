package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/testsupport"
)

// A machine with the package installed has a binary, a unit, and an empty
// database. What the package cannot know is the uid of the caller, so the
// path from there to a working dagger is three commands of the binary's
// own: migrate, bootstrap for that uid, and start. Nothing here touches
// the database except to count what bootstrap left.

// principalCount reads how many principals the database holds.
func principalCount(ctx context.Context, url string) int {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close(ctx) //nolint:errcheck // single-statement session
	var n int
	Expect(conn.QueryRow(ctx, `SELECT count(*) FROM principals`).Scan(&n)).To(Succeed())
	return n
}

// admin runs one of the binary's database-facing commands to completion.
func admin(url string, args ...string) *gexec.Session {
	GinkgoHelper()
	session, err := gexec.Start(exec.Command(binary, append(args, "--database-url", url)...), GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	Eventually(session, 60*time.Second).Should(gexec.Exit(), "dg %v did not exit", args)
	return session
}

var _ = Describe("Bootstrapping a deployment", Label(testsupport.DBLabel), func() {
	It("Scenario: A clean machine becomes a working dagger", func(ctx SpecContext) {
		By("given the dagger binary and a reachable empty database")
		url := pg.EmptyDatabase(ctx)
		dir := GinkgoT().TempDir()
		uid := strconv.Itoa(os.Getuid())

		By("when dg migrate runs")
		session := admin(url, "migrate")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())

		By("and dg admin bootstrap runs for the caller's user id")
		session = admin(url, "admin", "bootstrap", uid, "acme")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())

		By("and the service is started, as the unit starts it")
		socket := filepath.Join(dir, "dg.sock")
		serve, err := gexec.Start(exec.Command(binary, "serve", "--socket", socket, "--database-url", url, "--shutdown-timeout", "5s"), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			serve.Terminate().Wait(10 * time.Second)
			serve.Kill()
		})
		Eventually(serve.Err, 30*time.Second).Should(gbytes.Say("database reachable; ready"))

		By("then dg ready answers the bootstrapped user with an empty ready set and no error")
		me := face{name: "host", flags: []string{"--socket", socket}, env: cleanEnv(dir), unix: true}
		project := me.createProject("Platform")
		Expect(project.GetSlug()).To(Equal(platform))
		res := &daggerv1.ReadyResponse{}
		me.call(res, "ready", "--project", platform)
		Expect(res.GetNodes()).To(BeEmpty())
	})

	It("Scenario: Bootstrap is safe against an already-seeded database", func(ctx SpecContext) {
		By("given a database that has already been bootstrapped")
		url := pg.EmptyDatabase(ctx)
		Expect(admin(url, "migrate").ExitCode()).To(BeZero())
		Expect(admin(url, "admin", "bootstrap", "1000", "acme").ExitCode()).To(BeZero())
		before := principalCount(ctx, url)

		By("when dg admin bootstrap is run again with the same user id")
		session := admin(url, "admin", "bootstrap", "1000", "acme")

		By("then it exits zero and the principal count is unchanged")
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		Expect(principalCount(ctx, url)).To(Equal(before))
	})
})
