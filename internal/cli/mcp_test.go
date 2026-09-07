package cli_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"google.golang.org/protobuf/reflect/protoreflect"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/cli"
	"github.com/ivy/dagger/internal/client"
	"github.com/ivy/dagger/internal/codec"
)

// mcpSession drives one dg mcp server over its stdio: JSON-RPC requests
// in, responses matched by id out. The server runs either in-process
// through cli.Run, or as the built binary when a spec needs to kill it.
type mcpSession struct {
	stdin  io.WriteCloser
	stderr *bytes.Buffer
	// exit receives the exit code once the server has stopped.
	exit chan int

	mu      sync.Mutex
	pending map[int64]chan map[string]any
	next    int64
}

// newMCPSession wires a session to the given streams and starts reading
// responses.
func newMCPSession(stdin io.WriteCloser, stdout io.Reader, stderr *bytes.Buffer, exit chan int) *mcpSession {
	m := &mcpSession{stdin: stdin, stderr: stderr, exit: exit, pending: map[int64]chan map[string]any{}}
	go func() {
		defer GinkgoRecover()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for scanner.Scan() {
			var res struct {
				ID     int64          `json:"id"`
				Result map[string]any `json:"result"`
				Error  map[string]any `json:"error"`
			}
			Expect(json.Unmarshal(scanner.Bytes(), &res)).To(Succeed(), "stdout was not JSON-RPC: %s", scanner.Text())
			m.mu.Lock()
			ch := m.pending[res.ID]
			delete(m.pending, res.ID)
			m.mu.Unlock()
			if ch != nil {
				if res.Error != nil {
					ch <- map[string]any{"error": res.Error}
				} else {
					ch <- res.Result
				}
			}
		}
	}()
	return m
}

// send writes one request and returns its result, or {"error": ...} for a
// JSON-RPC error.
func (m *mcpSession) send(method string, params any) map[string]any {
	GinkgoHelper()
	m.mu.Lock()
	m.next++
	id := m.next
	ch := make(chan map[string]any, 1)
	m.pending[id] = ch
	m.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	Expect(err).NotTo(HaveOccurred())
	_, err = m.stdin.Write(append(raw, '\n'))
	Expect(err).NotTo(HaveOccurred())
	var res map[string]any
	Eventually(ch, 10*time.Second).Should(Receive(&res), "no response to %s", method)
	return res
}

// notify writes one notification, which gets no reply.
func (m *mcpSession) notify(method string) {
	GinkgoHelper()
	_, err := fmt.Fprintf(m.stdin, `{"jsonrpc":"2.0","method":%q}`+"\n", method)
	Expect(err).NotTo(HaveOccurred())
}

// initialize performs the handshake as a client calling itself claude-code.
func (m *mcpSession) initialize() map[string]any {
	GinkgoHelper()
	res := m.send("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "claude-code", "version": "1.0"},
	})
	Expect(res).NotTo(HaveKey("error"))
	m.notify("notifications/initialized")
	return res
}

// toolResult is one tools/call answer.
type toolResult struct {
	text    string
	isError bool
}

// decode parses the text as JSON.
func (r toolResult) decode() map[string]any {
	GinkgoHelper()
	var out map[string]any
	Expect(json.Unmarshal([]byte(r.text), &out)).To(Succeed(), "tool text was not JSON: %s", r.text)
	return out
}

// call invokes one tool.
func (m *mcpSession) call(tool string, args any) toolResult {
	GinkgoHelper()
	res := m.send("tools/call", map[string]any{"name": tool, "arguments": args})
	Expect(res).NotTo(HaveKey("error"), "tools/call %s was a protocol error: %v", tool, res["error"])
	content, _ := res["content"].([]any)
	Expect(content).To(HaveLen(1))
	first, _ := content[0].(map[string]any)
	Expect(first).To(HaveKeyWithValue("type", "text"))
	isError, _ := res["isError"].(bool)
	return toolResult{text: first["text"].(string), isError: isError}
}

// tools lists the advertised tools by name.
func (m *mcpSession) tools() map[string]map[string]any {
	GinkgoHelper()
	res := m.send("tools/list", map[string]any{})
	Expect(res).NotTo(HaveKey("error"))
	list, _ := res["tools"].([]any)
	out := map[string]map[string]any{}
	for _, t := range list {
		tool, _ := t.(map[string]any)
		out[tool["name"].(string)] = tool
	}
	return out
}

// closeStdin ends the session the way a harness does and waits for exit.
func (m *mcpSession) closeStdin() int {
	GinkgoHelper()
	Expect(m.stdin.Close()).To(Succeed())
	var code int
	Eventually(m.exit, 10*time.Second).Should(Receive(&code))
	return code
}

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
})

var _ = Describe("dg mcp", Label("unit"), func() {
	var (
		s      *stub
		socket string
	)

	// startMCP runs the server in-process on pipes.
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
	})

	Describe("Scenario: A session claims and releases", func() {
		It("A session claims and releases", func() {
			By("given the MCP server started with a configured transport")
			s.newNode("Claimable work")
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			By("when the client claims a node")
			res := m.call("claim", map[string]any{"labels": []string{}})
			Expect(res.isError).To(BeFalse(), res.text)
			claimed := res.decode()
			Expect(claimed["node"]).To(HaveKeyWithValue("title", "Claimable work"))
			leaseID := claimed["lease"].(map[string]any)["id"].(string)
			Expect(leaseID).NotTo(BeEmpty())
			sent := last[*daggerv1.ClaimNextRequest](s)
			Expect(sent.GetHarness()).To(Equal("claude-code"), "the harness is what the client called itself")
			Expect(sent.GetSessionRef()).NotTo(BeEmpty(), "the server mints the session reference")

			By("and then closes stdin")
			Expect(m.closeStdin()).To(Equal(0), m.stderr.String())

			By("then the lease is released with a reason")
			released := last[*daggerv1.ReleaseRequest](s)
			Expect(released.GetLeaseId()).To(Equal(leaseID))
			Expect(released.GetReason()).To(Equal(daggerv1.ReleaseReason_RELEASE_REASON_CRASH))
			Expect(released.GetDetail()).To(ContainSubstring("stdin closed"))
			Expect(s.nodes[0].GetReady()).To(BeTrue(), "the node is back in the ready set")
		})
	})

	Describe("Scenario: Tool descriptions carry the reference syntax", func() {
		It("Tool descriptions carry the reference syntax", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			By("when the client lists tools")
			tools := m.tools()

			By("then the patch tool description shows an example using an alias")
			patch := tools["apply_patch"]
			Expect(patch).NotTo(BeNil())
			description := patch["description"].(string)
			Expect(description).To(ContainSubstring(`"alias": "spike"`))
			Expect(description).To(ContainSubstring(`"blocked_by": ["spike"]`))
			Expect(description).To(ContainSubstring(`"from": "spike"`))
			Expect(description).To(ContainSubstring("#7"), "and the reference forms")
			Expect(description).To(ContainSubstring("slug#7"))
			Expect(description).To(ContainSubstring("org/slug#7"))
		})
	})

	Describe("Scenario: Each tool asks for the view its purpose needs", func() {
		It("Each tool asks for the view its purpose needs", func() {
			s.newNode("Viewed")
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			By("given the tools advertise a view argument with the short names")
			tools := m.tools()
			for _, name := range []string{"ready", "search", "show_node", "apply_patch", "complete", "comment"} {
				props := tools[name]["inputSchema"].(map[string]any)["properties"].(map[string]any)
				Expect(props).To(HaveKey("view"), name)
				Expect(props["view"].(map[string]any)["enum"]).To(ConsistOf("refs", "summary", "full"), name)
			}

			By("when each tool is called without one")
			Expect(m.call("ready", map[string]any{}).isError).To(BeFalse())
			Expect(m.call("search", map[string]any{"query": "viewed"}).isError).To(BeFalse())
			Expect(m.call("show_node", map[string]any{"ref": "#1"}).isError).To(BeFalse())
			Expect(m.call("apply_patch", map[string]any{"ops": []any{map[string]any{"create_node": map[string]any{"title": "A", "body": "long"}}}}).isError).To(BeFalse())
			applied := last[*daggerv1.ApplyPatchRequest](s)
			Expect(m.call("comment", map[string]any{"ref": "#1", "body": "a note"}).isError).To(BeFalse())
			commented := last[*daggerv1.ApplyPatchRequest](s)
			Expect(m.call("complete", map[string]any{"ref": "#1", "result": "done"}).isError).To(BeFalse())
			completed := last[*daggerv1.ApplyPatchRequest](s)

			By("then ready and search ask for a summary, show_node for full, and the writes for refs")
			Expect(last[*daggerv1.ReadyRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))
			Expect(last[*daggerv1.SearchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))
			Expect(last[*daggerv1.GetNodeRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_FULL))
			Expect(applied.GetView()).To(Equal(daggerv1.View_VIEW_REFS))
			Expect(applied.GetOps()[0].GetCreateNode().GetBody()).To(Equal("long"), "the view is taken out of the arguments, the patch is not")
			Expect(commented.GetView()).To(Equal(daggerv1.View_VIEW_REFS))
			Expect(completed.GetView()).To(Equal(daggerv1.View_VIEW_REFS))

			By("and a view given by short name is sent, and one that is not a view is refused")
			Expect(m.call("apply_patch", map[string]any{"view": "full", "ops": []any{map[string]any{"create_node": map[string]any{"title": "B"}}}}).isError).To(BeFalse())
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_FULL))
			Expect(m.call("ready", map[string]any{"view": "refs"}).isError).To(BeFalse())
			Expect(last[*daggerv1.ReadyRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_REFS))
			Expect(m.call("show_node", map[string]any{"ref": "#1", "view": "summary"}).isError).To(BeFalse())
			Expect(last[*daggerv1.GetNodeRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))
			Expect(m.call("apply_patch", map[string]any{"view": "VIEW_FULL", "ops": []any{map[string]any{"create_node": map[string]any{"title": "C"}}}}).isError).To(BeFalse(), "the arguments are a message in protojson, so the enum's wire spelling is accepted too")
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_FULL))
			res := m.call("ready", map[string]any{"view": "huge"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring(`view: "huge" is not one of full, refs, summary`))
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("Enum arguments declare their values", func() {
		It("every tool argument that maps to a proto enum lists the short forms", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			By("given the enum-valued fields of the request messages, by name")
			enums := requestEnums()
			Expect(enums).NotTo(BeEmpty())

			By("when the client lists the tool schemas")
			checked := 0
			for name, tool := range m.tools() {
				schema, _ := tool["inputSchema"].(map[string]any)
				props, _ := schema["properties"].(map[string]any)
				for prop, raw := range props {
					ed, ok := enums[prop]
					if !ok {
						continue
					}
					checked++

					By("then " + name + "." + prop + " carries the short forms of its enum")
					property, _ := raw.(map[string]any)
					values, _ := property["enum"].([]any)
					Expect(values).NotTo(BeEmpty(), "%s: %s declares no enum values", name, prop)
					legal := codec.EnumNames(ed)
					for _, v := range values {
						Expect(legal).To(ContainElement(v), "%s: %s: %v is not a short name of %s", name, prop, v, ed.FullName())
					}
				}
			}
			Expect(checked).To(BeNumerically(">=", 3), "no enum argument was found to check")
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("Scenario: The apply_patch description names exactly the PatchOp operations", func() {
		It("The apply_patch description names exactly the PatchOp operations", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			By("given the operations the PatchOp oneof declares")
			fields := daggerv1.File_dagger_v1_service_proto.Messages().ByName("PatchOp").Oneofs().ByName("op").Fields()
			ops := map[string]bool{}
			for i := 0; i < fields.Len(); i++ {
				ops[string(fields.Get(i).Name())] = true
			}
			Expect(ops).NotTo(BeEmpty())

			By("when the client lists tools")
			description := m.tools()["apply_patch"]["description"].(string)

			By("then every operation appears in the description")
			for op := range ops {
				Expect(description).To(ContainSubstring(op))
			}

			By("and no word ending in _node or _edge in the description is absent from the oneof")
			for _, word := range regexp.MustCompile(`\b\w+_(?:node|edge)\b`).FindAllString(description, -1) {
				Expect(ops).To(HaveKey(word), "the description names %q, which is not a PatchOp operation", word)
			}
		})
	})

	Describe("the server starts, advertises its tools, and shuts down on EOF", func() {
		It("answers initialize with its identity and the tools capability", func() {
			m := startMCP("--socket", socket)
			res := m.initialize()
			Expect(res).To(HaveKeyWithValue("protocolVersion", "2025-06-18"))
			Expect(res["serverInfo"]).To(HaveKeyWithValue("name", "dg"))
			Expect(res["capabilities"]).To(HaveKey("tools"))
			Expect(res["instructions"]).To(ContainSubstring("#7"))
			Expect(m.send("ping", nil)).To(BeEmpty())
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("advertises every tool the work order names, each with an input schema", func() {
			m := startMCP("--socket", socket)
			m.initialize()
			tools := m.tools()
			Expect(tools).To(HaveLen(10))
			for _, name := range []string{"search", "show_node", "explain", "apply_patch", "ready", "claim", "heartbeat", "release", "complete", "comment"} {
				Expect(tools).To(HaveKey(name))
				Expect(tools[name]["description"]).NotTo(BeEmpty())
				Expect(tools[name]["inputSchema"]).To(HaveKeyWithValue("type", "object"))
			}
			Expect(tools["show_node"]["inputSchema"].(map[string]any)["required"]).To(ConsistOf("ref"))
		})

		It("shuts down cleanly on EOF with nothing but protocol messages on stdout", func() {
			m := startMCP("--socket", socket)
			m.initialize()
			Expect(m.closeStdin()).To(Equal(0))
			Expect(m.stderr.String()).To(ContainSubstring("stdin closed"))
			Expect(m.stderr.String()).NotTo(ContainSubstring("released lease"), "nothing was held")
		})

		It("keeps working without an initialize, for a client that skips it", func() {
			m := startMCP("--socket", socket)
			Expect(m.tools()).To(HaveKey("ready"))
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("tool inputs use the same reference syntax as the CLI", func() {
		It("sends a qualified reference as written and resolves a bare one in the current project", func() {
			s.newNode("Referenced")
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			res := m.call("show_node", map[string]any{"ref": "acme/platform#1"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(last[*daggerv1.GetNodeRequest](s).GetNode()).To(Equal("acme/platform#1"))

			res = m.call("show_node", map[string]any{"ref": "#1"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(last[*daggerv1.GetNodeRequest](s).GetNode()).To(Equal("#1"))
			Expect(last[*daggerv1.GetNodeRequest](s).GetProject()).To(Equal("platform"))

			res = m.call("show_node", map[string]any{"ref": "1"})
			Expect(res.isError).To(BeFalse(), "a bare number is accepted as #number: %s", res.text)
			Expect(last[*daggerv1.GetNodeRequest](s).GetNode()).To(Equal("#1"))

			Expect(m.closeStdin()).To(Equal(0))
		})

		It("rejects a malformed reference the way the CLI does", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			res := m.call("explain", map[string]any{"ref": "seven"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring(client.ErrBadNodeRef.Error()))
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("needs a project for a bare reference and says how to give one", func() {
			m := startMCP("--socket", socket)
			m.initialize()
			res := m.call("show_node", map[string]any{"ref": "#1"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("slug#number"))

			By("a qualified reference needs no configured project")
			s.newNode("Qualified")
			res = m.call("complete", map[string]any{"ref": "platform#1", "result": "done"})
			Expect(res.isError).To(BeFalse(), res.text)
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetOps()[0].GetCompleteNode().GetNode()).To(Equal("platform#1"))
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("applies a patch with aliases exactly as dg patch apply does", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			res := m.call("apply_patch", map[string]any{"ops": []any{
				map[string]any{"create_node": map[string]any{"alias": "spike", "title": "Spike"}},
				map[string]any{"create_node": map[string]any{"alias": "impl", "title": "Implement", "blocked_by": []string{"spike"}}},
				map[string]any{"add_edge": map[string]any{"from": "spike", "to": "impl"}},
			}})
			Expect(res.isError).To(BeFalse(), res.text)
			out := res.decode()
			Expect(out["nodes"]).To(HaveLen(2))
			Expect(out["aliases"]).To(HaveKeyWithValue("spike", "platform#1"))
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetRequestId()).NotTo(BeEmpty(), "a request id is minted")
			Expect(sent.GetOps()[2].GetAddEdge().GetFrom()).To(Equal("spike"))
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("errors return as tool errors with actionable text", func() {
		It("relays the service's error with its code and what to do next", func() {
			s.newNode("Stale")
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			res := m.call("complete", map[string]any{"ref": "#1", "result": "done", "lease_id": expiredLease})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("failed_precondition"))
			Expect(res.text).To(ContainSubstring("claimed again"))
			Expect(res.text).NotTo(ContainSubstring("goroutine"))
			Expect(res.text).NotTo(ContainSubstring(".go:"))
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("names a bad argument rather than ignoring it", func() {
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()

			res := m.call("ready", map[string]any{"min_priority": "urgent"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring(`min_priority: "urgent" is not one of p0, p1, p2, p3, p4`))

			res = m.call("ready", map[string]any{"priority": "p1"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring(`unknown field "priority"`))

			res = m.call("release", map[string]any{"lease_id": "01JX", "reason": "bored"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("reason"))

			res = m.call("release", map[string]any{"reason": "gate"})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("holds no lease"))
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("answers an unknown tool and an unknown method as protocol errors", func() {
			m := startMCP("--socket", socket)
			m.initialize()
			res := m.send("tools/call", map[string]any{"name": "teleport", "arguments": map[string]any{}})
			Expect(res["error"]).To(HaveKeyWithValue("message", ContainSubstring("unknown tool")))
			res = m.send("resources/list", nil)
			Expect(res["error"]).To(HaveKeyWithValue("code", BeNumerically("==", -32601)))
			Expect(m.closeStdin()).To(Equal(0))
		})
	})

	Describe("the lease lifetime is tied to the process", func() {
		It("heartbeats a claimed lease in the background and uses it by default", func() {
			s.newNode("Kept alive")
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			res := m.call("claim", map[string]any{"ttl": "900ms"})
			Expect(res.isError).To(BeFalse(), res.text)
			leaseID := res.decode()["lease"].(map[string]any)["id"].(string)

			By("the lease outlives its TTL because the server renews it")
			Eventually(func() *daggerv1.HeartbeatRequest {
				s.mu.Lock()
				defer s.mu.Unlock()
				for i := len(s.requests) - 1; i >= 0; i-- {
					if hb, ok := s.requests[i].(*daggerv1.HeartbeatRequest); ok {
						return hb
					}
				}
				return nil
			}, 5*time.Second).ShouldNot(BeNil())
			Expect(last[*daggerv1.HeartbeatRequest](s).GetLeaseId()).To(Equal(leaseID))
			Consistently(func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.expire()
				return s.nodes[0].GetClaimed()
			}, 2*time.Second, 100*time.Millisecond).Should(BeTrue(), "the node stayed claimed past the TTL")

			By("a comment on the node carries the held lease without being told")
			res = m.call("comment", map[string]any{"ref": "#1", "body": "progress"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetLeaseId()).To(Equal(leaseID))

			By("completing the node ends the lease, so shutdown releases nothing")
			res = m.call("complete", map[string]any{"ref": "#1", "result": "done"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetLeaseId()).To(Equal(leaseID))
			Expect(m.closeStdin()).To(Equal(0))
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, req := range s.requests {
				Expect(req).NotTo(BeAssignableToTypeOf(&daggerv1.ReleaseRequest{}))
			}
		})

		It("an explicit release ends the lease and shutdown does not release it again", func() {
			s.newNode("Gated")
			m := startMCP("--socket", socket, "--project", "platform")
			m.initialize()
			Expect(m.call("claim", map[string]any{}).isError).To(BeFalse())
			res := m.call("release", map[string]any{"reason": "gate", "detail": "needs a person"})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(res.decode()["lease"]).To(HaveKeyWithValue("release_reason", "gate"))
			Expect(m.closeStdin()).To(Equal(0))
			s.mu.Lock()
			defer s.mu.Unlock()
			releases := 0
			for _, req := range s.requests {
				if _, ok := req.(*daggerv1.ReleaseRequest); ok {
					releases++
				}
			}
			Expect(releases).To(Equal(1))
		})
	})

	Describe("transport is configurable", func() {
		It("reaches a service at a network address given with --address", func() {
			By("given the stub served over HTTP, as a tailnet address is")
			mux := http.NewServeMux()
			mux.Handle(daggerv1connect.NewGraphServiceHandler(s))
			mux.Handle(daggerv1connect.NewWorkServiceHandler(s))
			ts := httptest.NewServer(mux)
			DeferCleanup(ts.Close)
			s.newNode("Remote work")

			m := startMCP("--address", ts.URL, "--project", "platform")
			m.initialize()
			res := m.call("ready", map[string]any{})
			Expect(res.isError).To(BeFalse(), res.text)
			Expect(res.decode()["nodes"]).To(HaveLen(1))
			Expect(m.stderr.String()).To(ContainSubstring(`transport=http`))
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("reads the address from the environment", func() {
			GinkgoT().Setenv(client.EnvAddress, "http://127.0.0.1:1")
			m := startMCP("--project", "platform")
			m.initialize()
			res := m.call("ready", map[string]any{})
			Expect(res.isError).To(BeTrue())
			Expect(res.text).To(ContainSubstring("127.0.0.1:1"))
			Expect(res.text).To(ContainSubstring("could not be reached"))
			Expect(m.closeStdin()).To(Equal(0))
		})

		It("refuses to start on an address that is not a URL", func() {
			m := startMCP("--address", "dagger.tail1234.ts.net")
			var code int
			Eventually(m.exit, 10*time.Second).Should(Receive(&code))
			Expect(code).NotTo(BeZero())
			Expect(m.stderr.String()).To(ContainSubstring("must be an http or https URL"))
		})
	})

	Describe("as a process", func() {
		var binary string

		BeforeEach(func() {
			var err error
			binary, err = gexec.Build("github.com/ivy/dagger/cmd/dg")
			Expect(err).NotTo(HaveOccurred())
		})

		// startProcess runs the built binary as a harness would.
		startProcess := func(args ...string) (*mcpSession, *exec.Cmd) {
			GinkgoHelper()
			cmd := exec.Command(binary, append(args, "mcp")...)
			cmd.Env = append(os.Environ(), "DG_CONFIG=", "DG_ADDRESS=", "DG_SOCKET=", "DG_PROJECT=")
			stdin, err := cmd.StdinPipe()
			Expect(err).NotTo(HaveOccurred())
			stdout, err := cmd.StdoutPipe()
			Expect(err).NotTo(HaveOccurred())
			stderr := &bytes.Buffer{}
			cmd.Stderr = io.MultiWriter(stderr, GinkgoWriter)
			Expect(cmd.Start()).To(Succeed())
			exit := make(chan int, 1)
			go func() {
				defer GinkgoRecover()
				err := cmd.Wait()
				var exitErr *exec.ExitError
				switch {
				case err == nil:
					exit <- 0
				case errors.As(err, &exitErr):
					exit <- exitErr.ExitCode()
				default:
					Fail("wait: " + err.Error())
				}
			}()
			DeferCleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
				}
			})
			return newMCPSession(stdin, stdout, stderr, exit), cmd
		}

		Describe("Scenario: A hard kill does not strand work", func() {
			It("A hard kill does not strand work", func() {
				By("given a claimed node")
				s.newNode("Stranded?")
				m, cmd := startProcess("--socket", socket, "--project", "platform")
				m.initialize()
				res := m.call("claim", map[string]any{"ttl": "500ms"})
				Expect(res.isError).To(BeFalse(), res.text)
				Expect(s.nodes[0].GetClaimed()).To(BeTrue())

				By("when the MCP process is killed without cleanup")
				Expect(cmd.Process.Kill()).To(Succeed())
				var code int
				Eventually(m.exit, 10*time.Second).Should(Receive(&code))
				Expect(code).NotTo(BeZero())

				By("then the lease expires and the node returns to the ready set")
				Eventually(func() []string {
					stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
					Expect(cli.Run([]string{"--socket", socket, "-p", "platform", "ready"}, strings.NewReader(""), stdout, stderr)).To(Equal(0), stderr.String())
					var res map[string]any
					Expect(json.Unmarshal(stdout.Bytes(), &res)).To(Succeed())
					nodes, _ := res["nodes"].([]any)
					titles := make([]string, 0, len(nodes))
					for _, n := range nodes {
						titles = append(titles, n.(map[string]any)["title"].(string))
					}
					return titles
				}, 5*time.Second, 50*time.Millisecond).Should(ConsistOf("Stranded?"))
				s.mu.Lock()
				defer s.mu.Unlock()
				for _, req := range s.requests {
					Expect(req).NotTo(BeAssignableToTypeOf(&daggerv1.ReleaseRequest{}), "nothing released it; it expired")
				}
			})
		})

		It("releases a held lease on SIGTERM", func() {
			s.newNode("Interrupted")
			m, cmd := startProcess("--socket", socket, "--project", "platform")
			m.initialize()
			res := m.call("claim", map[string]any{})
			Expect(res.isError).To(BeFalse(), res.text)
			leaseID := res.decode()["lease"].(map[string]any)["id"].(string)

			Expect(cmd.Process.Signal(syscall.SIGTERM)).To(Succeed())
			var code int
			Eventually(m.exit, 10*time.Second).Should(Receive(&code))
			Expect(code).To(BeZero(), m.stderr.String())

			released := last[*daggerv1.ReleaseRequest](s)
			Expect(released.GetLeaseId()).To(Equal(leaseID))
			Expect(released.GetReason()).To(Equal(daggerv1.ReleaseReason_RELEASE_REASON_CRASH))
			Expect(released.GetDetail()).To(ContainSubstring("signal received"))
			Expect(strings.TrimSpace(m.stderr.String())).NotTo(BeEmpty())
		})
	})
})

// requestEnums maps the name of every enum-valued field of a dagger.v1
// request message to the enum it takes, which is what a tool argument of
// that name has to declare.
func requestEnums() map[string]protoreflect.EnumDescriptor {
	out := map[string]protoreflect.EnumDescriptor{}
	for _, file := range []protoreflect.FileDescriptor{
		daggerv1.File_dagger_v1_dagger_proto,
		daggerv1.File_dagger_v1_service_proto,
	} {
		for i := range file.Messages().Len() {
			message := file.Messages().Get(i)
			if !strings.HasSuffix(string(message.Name()), "Request") {
				continue
			}
			for j := range message.Fields().Len() {
				field := message.Fields().Get(j)
				if field.Kind() == protoreflect.EnumKind {
					out[string(field.Name())] = field.Enum()
				}
			}
		}
	}
	return out
}
