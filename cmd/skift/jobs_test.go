package main_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/testsupport"
)

// The operator's side of skift's background jobs, with no serve running to
// work them. The jobs themselves run under serve; tests/acceptance covers
// that.
var _ = Describe("skift admin jobs, with no serve running", Label(testsupport.DBLabel), func() {
	It("lists expire-leases with its schedule and no run yet", func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()
		session := skift(binary, "admin", "jobs", "list", "--database-url", url)
		Expect(session.ExitCode()).To(BeZero(), "stderr: %s", session.Err.Contents())
		var statuses []map[string]any
		Expect(json.Unmarshal(session.Out.Contents(), &statuses)).To(Succeed(), "stdout: %s", session.Out.Contents())
		Expect(statuses).To(ContainElement(And(
			HaveKeyWithValue("name", "expire-leases"),
			HaveKeyWithValue("every", "1m0s"),
			Not(HaveKey("last_run")),
		)))
	})

	It("says nothing is working jobs rather than waiting forever, and leaves the run queued", func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()
		session := skift(binary, "admin", "jobs", "run", "expire-leases", "--wait", "--database-url", url)
		Expect(session.ExitCode()).To(Equal(cli.ExitFailure))
		Expect(string(session.Err.Contents())).To(ContainSubstring("nothing is working jobs: is skift serve running?"))
		var run map[string]any
		Expect(json.Unmarshal(session.Out.Contents(), &run)).To(Succeed(), "stdout: %s", session.Out.Contents())
		Expect(run).To(HaveKeyWithValue("outcome", "available"))
	})

	It("refuses a job the schedule does not carry, naming those it does", func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()
		session := skift(binary, "admin", "jobs", "run", "no-such-job", "--database-url", url)
		Expect(session.ExitCode()).To(Equal(cli.ExitFailure))
		Expect(string(session.Err.Contents())).To(SatisfyAll(
			ContainSubstring("no scheduled job named"),
			ContainSubstring("no-such-job"),
			ContainSubstring("the schedule has expire-leases"),
		))
	})
})
