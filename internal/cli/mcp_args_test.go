package cli_test

import (
	"bytes"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/client"
)

// Every tool reads its arguments before it sends anything, and what it
// says about a bad argument is the caller's next prompt. These specs
// check that each refusal names the argument and what it takes, and that
// nothing reached the service.
var _ = Describe("skift mcp argument diagnostics", Label("unit"), func() {
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
		s.newNode("More work")
	})

	It("refuses a bad argument by name, with what it takes, and sends nothing", func() {
		m := startMCP("--socket", socket, "--project", "platform")
		m.initialize()

		for _, c := range []struct {
			tool string
			args map[string]any
			says string
		}{
			{"search", map[string]any{}, "query is required"},
			{"search", map[string]any{"query": "auth", "view": "huge"}, `view: "huge" is not one of`},
			{"show_node", map[string]any{"ref": "nope"}, "nope"},
			{"show_node", map[string]any{"ref": "#1", "view": "huge"}, `view: "huge" is not one of`},
			{"inputs", map[string]any{"ref": "nope"}, "nope"},
			{"explain", map[string]any{"ref": "nope"}, "nope"},
			{"complete", map[string]any{"ref": "nope", "result": "done"}, "nope"},
			{"comment", map[string]any{"ref": "nope", "body": "hi"}, "nope"},
			{"ready", map[string]any{"assignee_kind": "robot"}, `assignee_kind: "robot" is not one of`},
			{"ready", map[string]any{"min_priority": "p9"}, `min_priority: "p9" is not one of`},
			{"claim", map[string]any{"min_priority": "urgent"}, `min_priority: "urgent" is not one of`},
			{"claim", map[string]any{"ttl": "later"}, `ttl: "later" is not a duration`},
			{"node_list", map[string]any{"status": "weird"}, `status: "weird" is not one of`},
			{"node_list", map[string]any{"view": "huge"}, `view: "huge" is not one of`},
			{"apply_patch", map[string]any{"ops": []any{}}, "ops is required"},
			{"apply_patch", map[string]any{"ops": []any{map[string]any{"create_node": map[string]any{"title": "A"}}}, "view": "huge"}, `view: "huge" is not one of`},
			{"release", map[string]any{"reason": "gate", "lease_id": 7}, "lease_id: expected a string"},
			{"project_list", map[string]any{"include_archived": "yes"}, "include_archived: expected true or false"},
			{"project_create", map[string]any{"title": "New", "priority": "high"}, `priority: "high" is not one of`},
			{"project_list", map[string]any{"page_size": "ten"}, "page_size: expected a number"},
			{"heartbeat", map[string]any{"ttl": 5}, "ttl: expected a string"},
			{"focus", map[string]any{"keep": []any{1}, "reason": "r"}, "keep: expected a string"},
		} {
			res := m.call(c.tool, c.args)
			Expect(res.isError).To(BeTrue(), "%s %v answered without error: %s", c.tool, c.args, res.text)
			Expect(res.text).To(ContainSubstring(c.says), "%s %v", c.tool, c.args)
		}
		Expect(s.requests).To(BeEmpty(), "a refused call reaches the service: %v", s.requests)
		Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
	})

	It("needs a project for a bare reference and for a project read, and says so", func() {
		m := startMCP("--socket", socket)
		m.initialize()
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"show_node", map[string]any{"ref": "#1"}},
			{"explain", map[string]any{"ref": "#1"}},
			{"project_show", map[string]any{}},
			{"apply_patch", map[string]any{"ops": []any{map[string]any{"create_node": map[string]any{"title": "A"}}}}},
		} {
			res := m.call(c.tool, c.args)
			Expect(res.isError).To(BeTrue(), "%s %v answered without error: %s", c.tool, c.args, res.text)
			Expect(res.text).To(ContainSubstring("project"), "%s %v", c.tool, c.args)
		}
		Expect(s.requests).To(BeEmpty())
		Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
	})

	It("passes a kind filter through to ready, claim, and node_list", func() {
		m := startMCP("--socket", socket, "--project", "platform")
		m.initialize()
		Expect(m.call("ready", map[string]any{"kind": "spike"}).isError).To(BeFalse())
		Expect(last[*skiftv1.ReadyRequest](s).GetFilter().GetKinds()).To(Equal([]string{"spike"}))
		Expect(m.call("node_list", map[string]any{"kind": "spike"}).isError).To(BeFalse())
		Expect(last[*skiftv1.ListNodesRequest](s).GetFilter().GetKinds()).To(Equal([]string{"spike"}))
		Expect(m.call("claim", map[string]any{"kind": "spike", "wait": "0s"}).isError).To(BeFalse())
		Expect(last[*skiftv1.ClaimNextRequest](s).GetFilter().GetKinds()).To(Equal([]string{"spike"}))
		Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
	})

	It("answers the protocol's own mistakes with JSON-RPC errors, and renders a denial with its details", func() {
		m := startMCP("--socket", socket, "--project", "platform")
		m.initialize()

		Expect(m.send("ping", nil)).NotTo(HaveKey("error"))

		rpcError := func(res map[string]any) (float64, string) {
			GinkgoHelper()
			Expect(res).To(HaveKey("error"), "%v", res)
			e := res["error"].(map[string]any)
			return e["code"].(float64), e["message"].(string)
		}
		code, msg := rpcError(m.send("nope/method", nil))
		Expect(code).To(BeEquivalentTo(-32601))
		Expect(msg).To(ContainSubstring(`"nope/method" is not supported`))

		code, msg = rpcError(m.send("tools/call", "not an object"))
		Expect(code).To(BeEquivalentTo(-32602))
		Expect(msg).To(HavePrefix("tools/call:"))

		code, msg = rpcError(m.send("tools/call", map[string]any{"name": "nope"}))
		Expect(code).To(BeEquivalentTo(-32602))
		Expect(msg).To(ContainSubstring(`unknown tool "nope"`))
		Expect(msg).To(ContainSubstring("tools/list"))

		code, msg = rpcError(m.send("initialize", 7))
		Expect(code).To(BeEquivalentTo(-32602))
		Expect(msg).To(HavePrefix("initialize:"))

		By("a notification gets no reply and does not stall the session")
		m.notify("notifications/initialized")
		Expect(m.send("ping", nil)).NotTo(HaveKey("error"))

		By("a denial says what the caller can do, and carries every detail the service attached")
		res := m.call("project_create", map[string]any{"title": "Forbidden"})
		Expect(res.isError).To(BeTrue())
		Expect(res.text).To(HavePrefix("project_create failed (permission_denied): no rule permits"))
		Expect(res.text).To(ContainSubstring("a person must grant access"))
		Expect(res.text).To(ContainSubstring(`detail: {"slug":"policy"`))
		Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
	})

	It("names every lease it holds when more than one could be meant", func() {
		m := startMCP("--socket", socket, "--project", "platform")
		m.initialize()
		first := m.call("claim", map[string]any{})
		Expect(first.isError).To(BeFalse(), first.text)
		second := m.call("claim", map[string]any{})
		Expect(second.isError).To(BeFalse(), second.text)
		one := first.decode()["lease"].(map[string]any)["id"].(string)
		two := second.decode()["lease"].(map[string]any)["id"].(string)

		res := m.call("heartbeat", map[string]any{})
		Expect(res.isError).To(BeTrue())
		Expect(res.text).To(ContainSubstring("holds 2 leases"))
		Expect(res.text).To(ContainSubstring(one))
		Expect(res.text).To(ContainSubstring(two))
		Expect(count[*skiftv1.HeartbeatRequest](s)).To(BeZero(), "the server's own renewals have not started yet")

		res = m.call("release", map[string]any{"reason": "budget"})
		Expect(res.isError).To(BeTrue())
		Expect(res.text).To(ContainSubstring("holds 2 leases"))
		Expect(m.closeStdin()).To(Equal(0), m.stderr.String())
	})
})
