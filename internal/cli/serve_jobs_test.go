package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/admin"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/testsupport"
)

// startServe runs `skift serve` in this process against databaseURL, on
// its own socket, and returns the function that shuts it down the way a
// signal does and waits for it to return.
func startServe(databaseURL string) (stop func()) {
	GinkgoHelper()
	socket := filepath.Join(GinkgoT().TempDir(), "skift.sock")
	root := cli.NewRootCommand(io.Discard, &syncBuffer{})
	root.SetArgs([]string{"serve",
		"--socket", socket,
		"--database-url", databaseURL,
		"--shutdown-timeout", "5s"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		done <- root.ExecuteContext(ctx)
	}()
	Eventually(func() error {
		_, err := os.Lstat(socket)
		return err
	}).Should(Succeed(), "the socket never appeared")
	return func() {
		GinkgoHelper()
		cancel()
		var err error
		Eventually(done, 15*time.Second).Should(Receive(&err), "serve never returned")
		Expect(err).NotTo(HaveOccurred())
	}
}

// countingJob is a scheduled job that does nothing but count its runs.
func countingJob(every time.Duration) (admin.Job, *atomic.Int64) {
	runs := &atomic.Int64{}
	return admin.Job{
		Name:  "spec-counter",
		Every: every,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	}, runs
}

var _ = Describe("Background jobs under skift serve", Label(testsupport.DBLabel), func() {
	It("runs a scheduled job once per period, however many serve processes share the database", func(ctx SpecContext) {
		const period = 2 * time.Second
		const periods = 5
		job, runs := countingJob(period)
		DeferCleanup(cli.RunJobsUnderServe(job))
		url := pg.Database(ctx).Config().ConnString()

		By("given two serve processes against one database")
		stopFirst := startServe(url)
		stopSecond := startServe(url)
		DeferCleanup(stopSecond)
		DeferCleanup(stopFirst)

		By("when the job has run once, and then several periods pass")
		Eventually(runs.Load, 30*time.Second).Should(BeNumerically(">=", 1), "the job never ran")
		from := runs.Load()
		time.Sleep(periods * period)

		By("then it ran about once per period, not once per process")
		// One elected leader schedules it, so the count tracks the periods
		// elapsed; two schedulers would roughly double it. One run of slack
		// either way covers a period boundary at each end of the window.
		ran := runs.Load() - from
		Expect(ran).To(BeNumerically(">=", periods-1), "too few runs: is anything scheduling it?")
		Expect(ran).To(BeNumerically("<=", periods+1), "too many runs: more than one process is scheduling it")
	})

	It("stops the jobs with the service, leaving nothing running", func(ctx SpecContext) {
		job, runs := countingJob(time.Second)
		DeferCleanup(cli.RunJobsUnderServe(job))
		url := pg.Database(ctx).Config().ConnString()
		before := runtime.NumGoroutine()

		By("given serve running its background jobs")
		stop := startServe(url)
		Eventually(runs.Load, 30*time.Second).Should(BeNumerically(">=", 1), "the job never ran")
		Expect(runtime.NumGoroutine()).To(BeNumerically(">", before), "serve and its jobs should be running goroutines")

		By("when it shuts down")
		stop()

		By("then no goroutine of serve or its jobs is left behind, and the job runs no more")
		// A few goroutines of the test harness itself come and go, so the
		// count may wobble by one or two; a leaked River client leaves
		// dozens.
		Eventually(runtime.NumGoroutine, 10*time.Second).Should(BeNumerically("<=", before+2))
		after := runs.Load()
		Consistently(runs.Load, 3*time.Second).Should(Equal(after))
	})
})
