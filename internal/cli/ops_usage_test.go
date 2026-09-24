package cli_test

import (
	"bytes"
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/client"
)

// The graph commands read their flags and references before they build a
// patch. A flag the command cannot read is a usage error that names the
// flag, and it never reaches the service.
var _ = Describe("skift graph command usage", Label("unit"), func() {
	var (
		s      *stub
		socket string
	)

	run := func(stdin io.Reader, args ...string) result {
		GinkgoHelper()
		r := result{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		r.code = cli.Run(append([]string{"--socket", socket, "-p", "platform"}, args...), stdin, r.stdout, r.stderr)
		return r
	}

	BeforeEach(func() {
		for _, k := range []string{client.EnvConfig, client.EnvSocket, client.EnvAddress, client.EnvProject} {
			GinkgoT().Setenv(k, "")
		}
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		s = newStub()
		socket = serve(s)
		s.newNode("Work")
		s.newNode("More work")
	})

	It("refuses a flag or reference it cannot read, naming it, before building a patch", func() {
		for _, c := range []struct {
			args []string
			says string
		}{
			{[]string{"create", "Title", "--priority", "high"}, "priority"},
			{[]string{"create", "Title", "--assignee-kind", "robot"}, "assignee-kind"},
			{[]string{"create", "Title", "--parent", "nope"}, "nope"},
			{[]string{"create", "Title", "--blocked-by", "nope"}, "nope"},
			{[]string{"update", "nope", "--title", "Renamed"}, "nope"},
			{[]string{"update", "#1", "--priority", "high"}, "priority"},
			{[]string{"update", "#1", "--assignee-kind", "robot"}, "assignee-kind"},
			{[]string{"update", "#1", "--parent", "nope"}, "nope"},
			{[]string{"link", "nope", "#1"}, "nope"},
			{[]string{"link", "#1", "nope"}, "nope"},
			{[]string{"link", "#1", "#2", "--kind", "bogus"}, "kind"},
			{[]string{"drop", "nope", "--reason", "superseded"}, "nope"},
			{[]string{"drop", "#1", "--reason", "bogus"}, "reason"},
			{[]string{"drop", "#1"}, "reason"},
			{[]string{"hold", "nope", "--reason", "parked"}, "nope"},
			{[]string{"unhold", "nope", "--reason", "back"}, "nope"},
		} {
			r := run(nil, c.args...)
			Expect(r.code).To(Equal(cli.ExitUsage), "%v: exit %d, stderr %s", c.args, r.code, r.stderr.String())
			Expect(r.stderrError().Message).To(ContainSubstring(c.says), "%v", c.args)
		}
		Expect(count[*skiftv1.ApplyPatchRequest](s)).To(BeZero(), "a refused command built a patch")
	})

	It("renders a refusal from the service with its code and every detail it carried", func() {
		r := run(nil, "project", "create", "Forbidden")
		Expect(r.code).To(Equal(cli.ExitFailure))
		e := r.stderrError()
		Expect(e.Code).To(Equal("permission_denied"))
		Expect(e.Message).To(ContainSubstring("no rule permits"))
		Expect(e.Details).To(HaveLen(1))
		Expect(string(e.Details[0])).To(ContainSubstring(`"policy"`))
	})

	It("builds the patch each command stands for", func() {
		r := run(nil, "create", "Spike the parser", "--kind", "spike", "--priority", "p1", "--parent", "#1", "--blocked-by", "#2", "--assignee-kind", "agent", "--body", "Find out.")
		Expect(r.code).To(Equal(0), r.stderr.String())
		op := last[*skiftv1.ApplyPatchRequest](s).GetOps()[0].GetCreateNode()
		Expect(op.GetTitle()).To(Equal("Spike the parser"))
		Expect(op.GetKind()).To(Equal("spike"))
		Expect(op.GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P1))
		Expect(op.GetParent()).To(Equal("#1"))
		Expect(op.GetBlockedBy()).To(Equal([]string{"#2"}))
		Expect(op.GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT))
		Expect(op.GetBody()).To(Equal("Find out."))

		r = run(nil, "link", "#1", "#2")
		Expect(r.code).To(Equal(0), r.stderr.String())
		edge := last[*skiftv1.ApplyPatchRequest](s).GetOps()[0].GetAddEdge()
		Expect(edge.GetFrom()).To(Equal("#1"))
		Expect(edge.GetTo()).To(Equal("#2"))

		r = run(nil, "drop", "#1", "--reason", "superseded", "--detail", "replaced by #2", "--cascade", "10")
		Expect(r.code).To(Equal(0), r.stderr.String())
		drop := last[*skiftv1.ApplyPatchRequest](s).GetOps()[0].GetDropNode()
		Expect(drop.GetNode()).To(Equal("#1"))
		Expect(drop.GetReason()).To(Equal(skiftv1.DropReason_DROP_REASON_SUPERSEDED))
		Expect(drop.GetDetail()).To(Equal("replaced by #2"))
		Expect(drop.GetCascade().GetMaxNodes()).To(BeEquivalentTo(10))
	})
})
