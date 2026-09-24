package cli_test

import (
	"bytes"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/client"
)

var _ = Describe("skift text output", Label("unit"), func() {
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
		r.code = cli.Run(append([]string{"--socket", socket, "-o", "text"}, args...), stdin, r.stdout, r.stderr)
		Expect(r.code).To(Equal(0), r.stderr.String())
		return r
	}

	BeforeEach(func() {
		for _, k := range []string{client.EnvConfig, client.EnvSocket, client.EnvAddress, client.EnvProject} {
			GinkgoT().Setenv(k, "")
		}
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		s = newStub()
		socket = serve(s)
	})

	It("explains every kind of reason on its own line, and a ready node as ready", func() {
		s.newNode("Work")
		s.newNode("Waiting")
		at := timestamppb.New(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
		s.reasons = []*skiftv1.Reason{
			{Reason: &skiftv1.Reason_NotOpen{NotOpen: &skiftv1.NotOpenReason{Status: skiftv1.Status_STATUS_DONE}}},
			{Reason: &skiftv1.Reason_Blocked{Blocked: &skiftv1.BlockedReason{Blocker: s.nodes[0], Gate: true}}},
			{Reason: &skiftv1.Reason_Leased{Leased: &skiftv1.LeasedReason{Lease: &skiftv1.Lease{Id: "01JL", Holder: "01JAGENT", ExpiresAt: at}}}},
			{Reason: &skiftv1.Reason_OpenChild{OpenChild: &skiftv1.OpenChildReason{Child: s.nodes[1]}}},
			{Reason: &skiftv1.Reason_Archived{Archived: &skiftv1.ArchivedReason{Project: &skiftv1.Project{Slug: "old"}}}},
			{Reason: &skiftv1.Reason_ProjectHeld{ProjectHeld: &skiftv1.ProjectHeldReason{Project: &skiftv1.Project{Slug: "platform", Hold: &skiftv1.Hold{Reason: "not this quarter"}}}}},
			{Reason: &skiftv1.Reason_Held{Held: &skiftv1.HeldReason{Number: 1, Title: "Work", Hold: &skiftv1.Hold{At: at, Reason: "parked in march"}}}},
			{Reason: &skiftv1.Reason_Held{Held: &skiftv1.HeldReason{Self: true, Hold: &skiftv1.Hold{At: at, Reason: "parked by hand"}}}},
		}

		out := run(nil, "-p", "platform", "explain", "#2").stdout.String()
		Expect(out).To(HavePrefix("#2 "))
		for _, line := range []string{
			"not open: done",
			"blocked by #1 Work (gate)",
			"leased by 01JAGENT until 2026-09-01T12:00:00Z",
			"open child #2 Waiting",
			"project old is archived",
			"project platform is held: not this quarter",
			"held by #1 Work since 2026-09-01T12:00:00Z: parked in march",
			"held since 2026-09-01T12:00:00Z: parked by hand",
		} {
			Expect(out).To(ContainSubstring(line + "\n"))
		}

		s.reasons = []*skiftv1.Reason{}
		Expect(run(nil, "-p", "platform", "explain", "#2").stdout.String()).To(HaveSuffix("ready\n"))
	})

	It("shows a node with its body and what it is expected to cost", func() {
		s.newNode("Work")
		s.nodes[0].Body = "# Plan\n\nDo the thing."
		out := run(nil, "-p", "platform", "node", "show", "#1").stdout.String()
		Expect(out).To(HavePrefix("#1 "))
		Expect(out).To(ContainSubstring("Work"))
		Expect(out).To(ContainSubstring("\n# Plan\n\nDo the thing.\n"))
		Expect(out).To(ContainSubstring("estimate claude-opus-5"))
		Expect(out).To(ContainSubstring("basis: two nodes of the same shape"))
		Expect(out).To(ContainSubstring("cost     claude-opus-5"))
		Expect(out).To(ContainSubstring("(2 records)"))
		Expect(out).To(ContainSubstring("expected claude-opus-5"))
		Expect(out).To(ContainSubstring("(1 nodes)"))
	})

	It("lists inputs with their size and body, and says when there are none", func() {
		s.newNode("Spike")
		s.newNode("Build")
		out := run(nil, "-p", "platform", "inputs", "#2").stdout.String()
		Expect(out).To(HavePrefix("#1 "))
		Expect(out).To(ContainSubstring("    15 bytes\n"))
		Expect(out).To(ContainSubstring("\nresult of Spike\n"))

		s = newStub()
		socket = serve(s)
		s.newNode("Alone")
		Expect(run(nil, "-p", "platform", "inputs", "#1").stdout.String()).To(Equal("no inputs\n"))
	})

	It("lists nodes one per line, and renders a patch as its nodes, aliases, and event count", func() {
		s.newNode("One")
		s.newNode("Two")
		out := run(nil, "-p", "platform", "node", "list").stdout.String()
		Expect(strings.Count(out, "\n")).To(Equal(2))
		Expect(out).To(ContainSubstring("#1 "))
		Expect(out).To(ContainSubstring("#2 "))

		patch := strings.NewReader(`{"ops": [{"create_node": {"alias": "spike", "title": "Spike"}}]}`)
		out = run(patch, "-p", "platform", "patch", "apply", "-").stdout.String()
		Expect(out).To(ContainSubstring("#3 "))
		Expect(out).To(ContainSubstring("alias spike = platform#3\n"))
		Expect(out).To(MatchRegexp(`1 events, request \S+\n`))
	})

	It("renders a lease as live or released, and search hits with their snippet", func() {
		s.newNode("Work")
		out := run(nil, "-p", "platform", "heartbeat", "01JX").stdout.String()
		Expect(out).To(MatchRegexp(`^lease 01JX expires in \S+\n$`))

		out = run(nil, "-p", "platform", "release", "01JX", "--reason", "gate").stdout.String()
		Expect(out).To(Equal("lease 01JX released: gate\n"))

		out = run(nil, "search", "Work").stdout.String()
		Expect(out).To(HavePrefix("#1 "))
		Expect(out).To(ContainSubstring("    <b>Work</b>\n"))
	})
})
