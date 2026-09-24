package cli_test

import (
	"bytes"
	"io"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/client"
)

var _ = Describe("skift mcp heartbeat, hold, and focus", Label("unit"), func() {
	var (
		s      *stub
		socket string
	)

	startMCP := func(args ...string) *mcpSession {
		GinkgoHelper()
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		stderr := &bytes.Buffer{}
		exit := make(chan int, 1)
		go func() {
			defer GinkgoRecover()
			exit <- cli.Run(append(args, "mcp"), inR, outW, stderr)
			Expect(outW.Close()).To(Succeed())
		}()
		m := newMCPSession(inW, outR, stderr, exit)
		DeferCleanup(func() {
			inW.Close() //nolint:errcheck // already closed by specs that end the session
		})
		return m
	}

	BeforeEach(func() {
		for _, k := range []string{client.EnvConfig, client.EnvSocket, client.EnvAddress, client.EnvProject} {
			GinkgoT().Setenv(k, "")
		}
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		s = newStub()
		socket = serve(s)
		s.newNode("Work")
	})

	Describe("heartbeat", func() {
		It("renews the session's one lease by default, or the lease named, with the TTL asked for", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			claimed := m.call("claim", map[string]any{})
			Expect(claimed.isError).To(BeFalse(), claimed.text)
			leaseID := claimed.decode()["lease"].(map[string]any)["id"].(string)

			By("with no arguments, the session's lease is renewed for the server's default")
			res := m.call("heartbeat", map[string]any{})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(res.decode()["lease"]).To(HaveKeyWithValue("id", leaseID))
			sent := last[*skiftv1.HeartbeatRequest](s)
			Expect(sent.GetLeaseId()).To(Equal(leaseID))
			Expect(sent.GetTtl()).To(BeNil())
			Expect(sent.GetRequestId()).NotTo(BeEmpty())

			By("a TTL is passed through")
			res = m.call("heartbeat", map[string]any{"ttl": "20m"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(last[*skiftv1.HeartbeatRequest](s).GetTtl().AsDuration()).To(Equal(20 * time.Minute))

			By("a lease claimed elsewhere is renewed by id")
			res = m.call("heartbeat", map[string]any{"lease_id": "01JELSEWHERE0000000000000"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(last[*skiftv1.HeartbeatRequest](s).GetLeaseId()).To(Equal("01JELSEWHERE0000000000000"))

			By("a TTL that is not a duration is refused before anything is sent")
			before := count[*skiftv1.HeartbeatRequest](s)
			res = m.call("heartbeat", map[string]any{"ttl": "soon"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("ttl"))
			Expect(res.text).To(ContainSubstring("30s or 5m"))
			Expect(count[*skiftv1.HeartbeatRequest](s)).To(Equal(before))
			Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
		})

		It("stops renewing, and forgets, a lease the server says is no longer live", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			claimed := m.call("claim", map[string]any{"ttl": "900ms"})
			Expect(claimed.isError).To(BeFalse(), claimed.text)
			leaseID := claimed.decode()["lease"].(map[string]any)["id"].(string)

			By("when the lease ends on the server between renewals")
			s.mu.Lock()
			s.leases[leaseID].reason = skiftv1.ReleaseReason_RELEASE_REASON_EXPIRED
			s.mu.Unlock()

			By("then the next renewal fails, the session drops the lease, and says so")
			Eventually(func() string {
				return m.call("heartbeat", map[string]any{}).text
			}, 10*time.Second, 200*time.Millisecond).Should(ContainSubstring("holds no lease"))
			Expect(m.stderr.String()).To(ContainSubstring("no longer live"))
			Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
		})

		It("refuses when the session holds no lease and none is named", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			res := m.call("heartbeat", map[string]any{})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("holds no lease"))
			Expect(count[*skiftv1.HeartbeatRequest](s)).To(BeZero())
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("hold", func() {
		It("parks a node as a patch of one operation, and releases it with release: true", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			res := m.call("hold", map[string]any{"node": "#1", "reason": "parked in march"})
			Expect(res.isError).To(BeFalse(), res.text)
			sent := last[*skiftv1.ApplyPatchRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetRequestId()).To(HaveLen(26), "minted when none is given")
			Expect(sent.GetView()).To(Equal(skiftv1.View_VIEW_SUMMARY))
			Expect(sent.GetOps()).To(HaveLen(1))
			Expect(sent.GetOps()[0].GetHoldNode().GetNode()).To(Equal("#1"))
			Expect(sent.GetOps()[0].GetHoldNode().GetReason()).To(Equal("parked in march"))

			res = m.call("hold", map[string]any{"node": "platform#1", "reason": "back", "release": true, "request_id": "unhold-1"})
			Expect(res.isError).To(BeFalse(), res.text)
			sent = last[*skiftv1.ApplyPatchRequest](s)
			Expect(sent.GetRequestId()).To(Equal("unhold-1"))
			Expect(sent.GetOps()[0].GetUnholdNode().GetNode()).To(Equal("platform#1"))
			Expect(sent.GetOps()[0].GetUnholdNode().GetReason()).To(Equal("back"))
			Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
		})

		It("refuses a malformed reference, a mistyped flag, and an unknown field before sending anything", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			res := m.call("hold", map[string]any{"node": "nope", "reason": "parked"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("nope"))

			res = m.call("hold", map[string]any{"node": "#1", "reason": "parked", "release": "yes"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("release: expected true or false"))

			res = m.call("hold", map[string]any{"node": "#1", "reason": "parked", "why": "extra"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring(`unknown field "why"`))
			Expect(res.text).To(ContainSubstring("node, reason, release, request_id"))

			Expect(count[*skiftv1.ApplyPatchRequest](s)).To(BeZero())
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("focus", func() {
		It("sweeps a project's nodes or the organization's projects, and releases the sweep with release: true", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			By("with a project, the keep-set names its nodes")
			res := m.call("focus", map[string]any{"keep": []string{"#1", "#2"}, "project": "platform", "reason": "this week"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(res.decode()).To(HaveKeyWithValue("held_count", BeNumerically("==", 2)))
			sent := last[*skiftv1.FocusRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetKeep()).To(Equal([]string{"#1", "#2"}))
			Expect(sent.GetReason()).To(Equal("this week"))
			Expect(sent.GetRequestId()).To(HaveLen(26))

			By("without one, the keep-set names projects and the current project is not assumed")
			res = m.call("focus", map[string]any{"keep": []string{"platform"}, "reason": "platform week", "request_id": "focus-org"})
			Expect(res.isError).To(BeFalse(), res.text)
			sent = last[*skiftv1.FocusRequest](s)
			Expect(sent.GetProject()).To(BeEmpty())
			Expect(sent.GetKeep()).To(Equal([]string{"platform"}))
			Expect(sent.GetRequestId()).To(Equal("focus-org"))

			By("release clears the scope's sweep, naming a group only to disambiguate")
			res = m.call("focus", map[string]any{"release": true, "project": "platform", "group": "01HZZZZZZZZZZZZZZZZZZZZZZF", "reason": "week over"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(res.decode()).To(HaveKeyWithValue("released_count", BeNumerically("==", 1)))
			cleared := last[*skiftv1.ClearFocusRequest](s)
			Expect(cleared.GetProject()).To(Equal("platform"))
			Expect(cleared.GetGroup()).To(Equal("01HZZZZZZZZZZZZZZZZZZZZZZF"))
			Expect(cleared.GetReason()).To(Equal("week over"))

			By("a keep-set that is not an array is refused with what it takes")
			res = m.call("focus", map[string]any{"keep": "#1", "reason": "this week"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("keep: expected an array of strings"))
			Expect(count[*skiftv1.FocusRequest](s)).To(Equal(2))
			Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
		})
	})
})
