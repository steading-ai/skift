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
	"github.com/steading-ai/skift/internal/codec"
)

var _ = Describe("skift hold, unhold, and focus", Label("unit"), func() {
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
		r.code = cli.Run(append([]string{"--socket", socket}, args...), stdin, r.stdout, r.stderr)
		return r
	}

	BeforeEach(func() {
		for _, k := range []string{client.EnvConfig, client.EnvSocket, client.EnvAddress, client.EnvProject} {
			GinkgoT().Setenv(k, "")
		}
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		s = newStub()
		socket = serve(s)
		s.newProject("platform", "Platform")
		s.newNode("Work")
	})

	Describe("hold and unhold", func() {
		It("park and release the current project with a reason, minting a request id", func() {
			r := run(nil, "-p", "platform", "project", "hold", "--reason", "not this quarter")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*skiftv1.HoldProjectRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetReason()).To(Equal("not this quarter"))
			Expect(sent.GetRequestId()).To(HaveLen(26))
			res := &skiftv1.HoldProjectResponse{}
			Expect(codec.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
			Expect(res.GetHeldCount()).To(BeEquivalentTo(1))
			Expect(res.GetProject().GetHold().GetReason()).To(Equal("not this quarter"))

			r = run(nil, "-p", "platform", "project", "unhold", "--reason", "green light")
			Expect(r.code).To(Equal(0), r.stderr.String())
			released := last[*skiftv1.UnholdProjectRequest](s)
			Expect(released.GetProject()).To(Equal("platform"))
			Expect(released.GetReason()).To(Equal("green light"))
			Expect(released.GetRequestId()).To(HaveLen(26))
			out := &skiftv1.UnholdProjectResponse{}
			Expect(codec.Unmarshal(r.stdout.Bytes(), out)).To(Succeed())
			Expect(out.GetProject().GetHold()).To(BeNil())
		})

		It("take the project as an argument and the request id from --request-id", func() {
			r := run(nil, "project", "hold", "platform", "--reason", "parked", "--request-id", "hold-1")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*skiftv1.HoldProjectRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetRequestId()).To(Equal("hold-1"))

			r = run(nil, "project", "unhold", "platform", "--reason", "back", "--request-id", "unhold-1")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(last[*skiftv1.UnholdProjectRequest](s).GetRequestId()).To(Equal("unhold-1"))
		})

		It("refuse to run without a reason, before reaching the service", func() {
			for _, verb := range []string{"hold", "unhold"} {
				r := run(nil, "-p", "platform", "project", verb)
				Expect(r.code).To(Equal(cli.ExitUsage), "%s: %s", verb, r.stderr.String())
				Expect(r.stderrError().Message).To(ContainSubstring("--reason"))
			}
			Expect(count[*skiftv1.HoldProjectRequest](s)).To(BeZero())
			Expect(count[*skiftv1.UnholdProjectRequest](s)).To(BeZero())
		})

		It("report a project the service does not know", func() {
			r := run(nil, "project", "hold", "nowhere", "--reason", "parked")
			Expect(r.code).To(Equal(cli.ExitFailure))
			Expect(r.stderrError().Message).To(ContainSubstring("project"))
		})

		It("render the hold and the count in text mode", func() {
			r := run(nil, "-p", "platform", "-o", "text", "project", "hold", "--reason", "not this quarter")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("platform"))
			Expect(r.stdout.String()).To(ContainSubstring("Platform"))
			Expect(r.stdout.String()).To(ContainSubstring("(held: not this quarter)"))
			Expect(r.stdout.String()).To(ContainSubstring("1 open nodes left the ready set"))

			r = run(nil, "-p", "platform", "-o", "text", "project", "unhold", "--reason", "green light")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("platform"))
			Expect(r.stdout.String()).NotTo(ContainSubstring("held"))
		})
	})

	Describe("hold <ref> and unhold <ref>", func() {
		It("park and release one node as a patch of one operation", func() {
			r := run(nil, "-p", "platform", "hold", "#1", "--reason", "parked", "--request-id", "hold-1")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*skiftv1.ApplyPatchRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetRequestId()).To(Equal("hold-1"))
			Expect(sent.GetOps()).To(HaveLen(1))
			Expect(sent.GetOps()[0].GetHoldNode().GetNode()).To(Equal("#1"))
			Expect(sent.GetOps()[0].GetHoldNode().GetReason()).To(Equal("parked"))

			r = run(nil, "unhold", "platform#1", "--reason", "back")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent = last[*skiftv1.ApplyPatchRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"), "the project comes from the reference")
			Expect(sent.GetRequestId()).To(HaveLen(26))
			Expect(sent.GetOps()[0].GetUnholdNode().GetNode()).To(Equal("platform#1"))
			Expect(sent.GetOps()[0].GetUnholdNode().GetReason()).To(Equal("back"))
		})

		It("refuse to run without a reason or a reference", func() {
			r := run(nil, "-p", "platform", "hold", "#1")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--reason"))

			r = run(nil, "-p", "platform", "unhold", "--reason", "back")
			Expect(r.code).To(Equal(cli.ExitUsage))

			Expect(count[*skiftv1.ApplyPatchRequest](s)).To(BeZero())
		})
	})

	Describe("focus", func() {
		It("keeps what is named and parks the rest of the project", func() {
			r := run(nil, "-p", "platform", "focus", "#1", "#2", "--reason", "this week")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*skiftv1.FocusRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetKeep()).To(Equal([]string{"#1", "#2"}))
			Expect(sent.GetReason()).To(Equal("this week"))
			Expect(sent.GetRequestId()).To(HaveLen(26))
			res := &skiftv1.FocusResponse{}
			Expect(codec.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
			Expect(res.GetHeldCount()).To(BeEquivalentTo(2))
			Expect(res.GetGroup()).NotTo(BeEmpty())
		})

		It("clears the active sweep with --clear, and --group only with it", func() {
			r := run(nil, "-p", "platform", "focus", "--clear", "--reason", "week over")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*skiftv1.ClearFocusRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetGroup()).To(BeEmpty())
			Expect(sent.GetReason()).To(Equal("week over"))
			res := &skiftv1.ClearFocusResponse{}
			Expect(codec.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
			Expect(res.GetReleasedCount()).To(BeEquivalentTo(1))

			r = run(nil, "-p", "platform", "focus", "--clear", "--group", "01HZZZZZZZZZZZZZZZZZZZZZZF", "--reason", "week over")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(last[*skiftv1.ClearFocusRequest](s).GetGroup()).To(Equal("01HZZZZZZZZZZZZZZZZZZZZZZF"))

			r = run(nil, "-p", "platform", "focus", "#1", "--group", "01HZZZZZZZZZZZZZZZZZZZZZZF", "--reason", "this week")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--group"))
		})

		It("refuses a keep-set with --clear, an empty keep-set without it, and a missing reason", func() {
			r := run(nil, "-p", "platform", "focus", "--clear", "#1", "--reason", "week over")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--clear"))

			r = run(nil, "-p", "platform", "focus", "--reason", "this week")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--clear"))

			r = run(nil, "-p", "platform", "focus", "#1")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--reason"))

			Expect(count[*skiftv1.FocusRequest](s)).To(BeZero())
			Expect(count[*skiftv1.ClearFocusRequest](s)).To(BeZero())
		})

		It("renders the sweep's counts in text mode", func() {
			r := run(nil, "-p", "platform", "-o", "text", "focus", "#1", "--reason", "this week")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("1 held, 0 released"))

			r = run(nil, "-p", "platform", "-o", "text", "focus", "--clear", "--reason", "week over")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("focus cleared: 1 released"))
		})
	})

	Describe("text renderings", func() {
		It("show a project list, a claimed lease, and the reasons a node waits", func() {
			s.newNode("Blocked work")

			r := run(nil, "-o", "text", "project", "list")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(HavePrefix("platform"))
			Expect(r.stdout.String()).To(ContainSubstring("Platform"))

			r = run(nil, "-p", "platform", "-o", "text", "claim", "--harness", "spec")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("#1"))
			Expect(r.stdout.String()).To(MatchRegexp(`lease \S+ expires in `))

			r = run(nil, "-p", "platform", "-o", "text", "explain", "#2")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("#2"))
			Expect(r.stdout.String()).To(ContainSubstring("blocked by #1 Work"))
		})
	})
})
