package acceptance_test

import (
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/testsupport"
)

// A snapshot is the way back from a release that misbehaved after
// migrating (docs/adrs/018-unattended-upgrades.md). Restoring one gives a
// database the service picks up where the snapshot left off.
var _ = Describe("Restoring a snapshot", Label(testsupport.DBLabel), func() {
	It("Scenario: A restored database accepts new work", func(ctx SpecContext) {
		By("given a database restored from a snapshot")
		d := deploy(ctx)
		d.laptop.createProject("Platform")
		created := d.laptop.patch(request(platform, createOp("a", "One"), createOp("b", "Two"), createOp("c", "Three")))
		Expect(numbers(created.GetNodes())).To(Equal([]int64{1, 2, 3}))

		session := operate(d.databaseURL, "admin", "snapshot", "--dir", GinkgoT().TempDir())
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		path := strings.TrimSpace(string(session.Out.Contents()))

		url := pg.EmptyDatabase(ctx)
		session = operate(url, "admin", "restore", path)
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		restored, err := pgxpool.New(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(restored.Close)
		var highest int64
		Expect(restored.QueryRow(ctx, `SELECT max(number) FROM nodes`).Scan(&highest)).To(Succeed())
		Expect(highest).To(Equal(int64(3)))

		By("when a patch creates a node")
		again := deployOn(restored)
		res := again.laptop.patch(request(platform, createOp("d", "Four")))

		By("then its number follows the highest restored number")
		Expect(numbers(res.GetNodes())).To(Equal([]int64{highest + 1}))
	})
})
