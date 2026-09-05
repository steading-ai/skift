package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/cli"
	"github.com/ivy/dagger/internal/client"
)

// run executes dg with args against the stub's socket, with stdin as its
// input, and returns the exit code and both streams.
type result struct {
	code   int
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// decode parses stdout as one JSON object.
func (r result) decode() map[string]any {
	GinkgoHelper()
	var out map[string]any
	Expect(json.Unmarshal(r.stdout.Bytes(), &out)).To(Succeed(), "stdout was not one JSON object: %s", r.stdout.String())
	return out
}

// stderrError parses stderr as the structured error.
func (r result) stderrError() cli.Error {
	GinkgoHelper()
	var e cli.Error
	Expect(json.Unmarshal(r.stderr.Bytes(), &e)).To(Succeed(), "stderr was not one JSON object: %s", r.stderr.String())
	return e
}

var _ = Describe("dg commands", Label("unit"), func() {
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
	})

	Describe("Scenario: An agent decomposes work from a shell", func() {
		It("An agent decomposes work from a shell", func() {
			By("given a JSON patch on stdin creating two nodes and linking them")
			patch := strings.NewReader(`{
			  "project": "platform",
			  "ops": [
			    {"create_node": {"alias": "spike", "title": "Spike the parser"}},
			    {"create_node": {"alias": "impl", "title": "Implement the parser"}},
			    {"add_edge": {"from": "spike", "to": "impl"}}
			  ]
			}`)

			By(`when I run "dg patch apply -"`)
			r := run(patch, "patch", "apply", "-")

			By("then the response lists both created nodes with their numbers")
			Expect(r.stderr.String()).To(BeEmpty())
			res := &daggerv1.ApplyPatchResponse{}
			Expect(protojson.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
			Expect(res.GetNodes()).To(HaveLen(2))
			Expect(res.GetNodes()[0].GetNumber()).To(BeEquivalentTo(1))
			Expect(res.GetNodes()[0].GetTitle()).To(Equal("Spike the parser"))
			Expect(res.GetNodes()[1].GetNumber()).To(BeEquivalentTo(2))
			Expect(res.GetAliases()).To(HaveKeyWithValue("impl", "platform#2"))

			By("and the exit code is 0")
			Expect(r.code).To(Equal(0))

			By("and the patch reached the service with a minted request id and every operation")
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetRequestId()).To(HaveLen(26))
			Expect(sent.GetOps()).To(HaveLen(3))
			Expect(sent.GetOps()[2].GetAddEdge().GetFrom()).To(Equal("spike"))
		})

		It("fills the project from --project and the lease from --lease", func() {
			r := run(strings.NewReader(`{"ops": [{"create_node": {"title": "A"}}]}`),
				"--project", "platform", "patch", "apply", "-", "--lease", "01JLEASE0000000000000000001", "--request-id", "01JFIXED000000000000000000")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetLeaseId()).To(Equal("01JLEASE0000000000000000001"))
			Expect(sent.GetRequestId()).To(Equal("01JFIXED000000000000000000"))
		})

		It("reads the patch from a file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "patch.json")
			Expect(os.WriteFile(path, []byte(`{"project": "platform", "ops": [{"create_node": {"title": "From a file"}}]}`), 0o600)).To(Succeed())
			r := run(nil, "patch", "apply", path)
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["nodes"]).To(HaveLen(1))
		})

		It("rejects a patch that is not an ApplyPatchRequest as a usage error", func() {
			r := run(strings.NewReader(`{"ops": [{"teleport_node": {}}]}`), "-p", "platform", "patch", "apply", "-")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Code).To(Equal("invalid_argument"))
			Expect(r.stderrError().Message).To(ContainSubstring("teleport_node"))
		})

		It("needs a project from somewhere", func() {
			r := run(strings.NewReader(`{"ops": []}`), "patch", "apply", "-")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Code).To(Equal("invalid_argument"))
			Expect(r.stderrError().Message).To(ContainSubstring("--project"))
		})
	})

	Describe("Scenario: Output is parseable when piped", func() {
		It("Output is parseable when piped", func() {
			s.newNode("Ready work")

			By(`when I run "dg ready" with stdout redirected to a file`)
			path := filepath.Join(GinkgoT().TempDir(), "ready.json")
			file, err := os.Create(path)
			Expect(err).NotTo(HaveOccurred())
			stderr := &bytes.Buffer{}
			code := cli.Run([]string{"--socket", socket, "--project", "platform", "ready"}, strings.NewReader(""), file, stderr)
			Expect(file.Close()).To(Succeed())
			Expect(code).To(Equal(0), stderr.String())

			By("then the file contains valid JSON")
			raw, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(json.Valid(raw)).To(BeTrue(), "not JSON: %s", raw)
			res := &daggerv1.ReadyResponse{}
			Expect(protojson.Unmarshal(raw, res)).To(Succeed())
			Expect(res.GetNodes()).To(HaveLen(1))
			Expect(res.GetNodes()[0].GetTitle()).To(Equal("Ready work"))
		})

		It("renders text on request, and JSON on request", func() {
			s.newNode("Ready work")
			text := run(nil, "-p", "platform", "-o", "text", "ready")
			Expect(text.code).To(Equal(0), text.stderr.String())
			Expect(text.stdout.String()).To(ContainSubstring("#1"))
			Expect(text.stdout.String()).To(ContainSubstring("Ready work"))
			Expect(json.Valid(text.stdout.Bytes())).To(BeFalse())

			asJSON := run(nil, "-p", "platform", "--output", "json", "ready")
			Expect(asJSON.code).To(Equal(0))
			Expect(asJSON.decode()).To(HaveKey("nodes"))
		})

		It("rejects an unknown output format", func() {
			r := run(nil, "-p", "platform", "-o", "yaml", "ready")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--output"))
		})
	})

	Describe("Scenario: A failed command is machine-readable", func() {
		It("A failed command is machine-readable", func() {
			s.newNode("Held work")

			By("when I complete a node with an expired lease")
			r := run(nil, "-p", "platform", "complete", "#1", "--lease", expiredLease, "--result", "done")

			By("then stderr contains a JSON object with a code and message")
			Expect(r.stdout.String()).To(BeEmpty())
			e := r.stderrError()
			Expect(e.Code).To(Equal(connect.CodeFailedPrecondition.String()))
			Expect(e.Message).To(ContainSubstring("lease is not live"))

			By("and the exit code is non-zero")
			Expect(r.code).NotTo(Equal(0))
		})

		It("reports a bad flag, a bad argument count, and an unknown command the same way", func() {
			for _, args := range [][]string{
				{"ready", "--bogus"},
				{"node", "show"},
				{"teleport"},
			} {
				r := run(nil, args...)
				Expect(r.code).NotTo(Equal(0), strings.Join(args, " "))
				Expect(r.stderrError().Code).NotTo(BeEmpty(), strings.Join(args, " "))
				Expect(r.stderrError().Message).NotTo(BeEmpty(), strings.Join(args, " "))
			}
		})

		It("reports an unreachable service as unavailable", func() {
			socket = filepath.Join(GinkgoT().TempDir(), "nobody.sock")
			r := run(nil, "-p", "platform", "ready")
			Expect(r.code).To(Equal(cli.ExitFailure))
			Expect(r.stderrError().Code).To(Equal("unavailable"))
			Expect(r.stderrError().Message).To(ContainSubstring(socket))
		})
	})

	Describe("node references", func() {
		BeforeEach(func() { s.newNode("Target") })

		DescribeTable("are accepted in every form and sent as written",
			func(ref, project, wantProject string) {
				args := []string{"node", "show", ref}
				if project != "" {
					args = append([]string{"-p", project}, args...)
				}
				r := run(nil, args...)
				Expect(r.code).To(Equal(0), r.stderr.String())
				sent := last[*daggerv1.GetNodeRequest](s)
				Expect(sent.GetNode()).To(Equal(ref))
				Expect(sent.GetProject()).To(Equal(wantProject))
			},
			Entry("#7 in the current project", "#7", "platform", "platform"),
			Entry("slug#7 with no current project", "oauth-spike#7", "", ""),
			Entry("org/slug#7 with no current project", "acme/oauth-spike#7", "", ""),
			Entry("slug#7 alongside a current project", "oauth-spike#7", "platform", "platform"),
		)

		It("refuses a bare #7 with no project, before any round trip", func() {
			r := run(nil, "explain", "#7")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--project"))
			Expect(s.requests).To(BeEmpty())
		})

		It("refuses a malformed reference", func() {
			r := run(nil, "-p", "platform", "node", "show", "seven")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("#7"))
		})

		It("derives the patch's project from a qualified reference", func() {
			r := run(nil, "link", "acme/platform#1", "acme/platform#2")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetProject()).To(Equal("acme/platform"))
			Expect(sent.GetOps()[0].GetAddEdge().GetFrom()).To(Equal("acme/platform#1"))
			Expect(sent.GetOps()[0].GetAddEdge().GetTo()).To(Equal("acme/platform#2"))
		})
	})

	Describe("ready --wait", func() {
		It("blocks over ClaimNext until work appears, then returns it claimed", func() {
			s.mu.Lock()
			s.readyGate = make(chan struct{})
			s.mu.Unlock()

			done := make(chan result, 1)
			go func() {
				defer GinkgoRecover()
				done <- run(nil, "-p", "platform", "ready", "--wait", "--label", "infra", "--ttl", "5m")
			}()

			By("nothing returns while nothing is ready")
			Consistently(done, 200*time.Millisecond).ShouldNot(Receive())
			Expect(last[*daggerv1.ClaimNextRequest](s).GetLabels()).To(Equal([]string{"infra"}))

			By("work appears")
			s.mu.Lock()
			s.newNode("Appeared")
			close(s.readyGate)
			s.mu.Unlock()

			var r result
			Eventually(done, 5*time.Second).Should(Receive(&r))
			Expect(r.code).To(Equal(0), r.stderr.String())
			res := &daggerv1.ClaimNextResponse{}
			Expect(protojson.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
			Expect(res.GetNode().GetTitle()).To(Equal("Appeared"))
			Expect(res.GetNode().GetClaimed()).To(BeTrue())
			Expect(res.GetLease().GetId()).NotTo(BeEmpty())

			sent := last[*daggerv1.ClaimNextRequest](s)
			Expect(sent.GetTtl().AsDuration()).To(Equal(5 * time.Minute))
			Expect(sent.GetHarness()).To(Equal("dg"))
		})

		It("gives up after a bounded wait with an empty response", func() {
			started := time.Now()
			r := run(nil, "-p", "platform", "ready", "--wait=100ms")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(time.Since(started)).To(BeNumerically(">=", 100*time.Millisecond))
			Expect(r.decode()).NotTo(HaveKey("node"))
		})

		It("without --wait lists the ready set and claims nothing", func() {
			s.newNode("Ready work")
			r := run(nil, "-p", "platform", "ready", "--min-priority", "p1", "--kind", "bug")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*daggerv1.ReadyRequest](s)
			Expect(sent.GetMinPriority()).To(Equal(daggerv1.Priority_PRIORITY_P1))
			Expect(sent.GetKind()).To(Equal("bug"))
			Expect(s.nodes[0].GetClaimed()).To(BeFalse())
		})
	})

	Describe("every command emits JSON", func() {
		It("project create, list, show, archive", func() {
			r := run(nil, "project", "create", "Platform", "--slug", "platform", "--priority", "p1")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["project"]).To(HaveKeyWithValue("slug", "platform"))
			Expect(last[*daggerv1.CreateProjectRequest](s).GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P1))

			r = run(nil, "project", "list", "--include-archived", "--page-size", "5")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["projects"]).To(HaveLen(1))
			Expect(r.decode()).To(HaveKeyWithValue("next_page_token", "next"))
			Expect(last[*daggerv1.ListProjectsRequest](s).GetPageSize()).To(BeEquivalentTo(5))

			r = run(nil, "project", "show", "platform")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["project"]).To(HaveKeyWithValue("title", "Platform"))

			r = run(nil, "-p", "platform", "project", "archive")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["project"]).To(HaveKey("archived_at"))
		})

		It("create, link, node show, explain", func() {
			r := run(nil, "-p", "platform", "create", "Spike", "--kind", "spike", "--priority", "p0", "-l", "infra", "--body", "-")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["nodes"]).To(HaveLen(1))
			op := last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetCreateNode()
			Expect(op.GetKind()).To(Equal("spike"))
			Expect(op.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P0))
			Expect(op.GetLabels()).To(Equal([]string{"infra"}))

			r = run(strings.NewReader("# Body\n"), "-p", "platform", "create", "Impl", "--blocked-by", "#1", "--body", "-", "--assignee-kind", "user")
			Expect(r.code).To(Equal(0), r.stderr.String())
			op = last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetCreateNode()
			Expect(op.GetBody()).To(Equal("# Body\n"))
			Expect(op.GetBlockedBy()).To(Equal([]string{"#1"}))
			Expect(op.GetAssigneeKind()).To(Equal(daggerv1.AssigneeKind_ASSIGNEE_KIND_USER))

			r = run(nil, "-p", "platform", "link", "#1", "#2")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()).NotTo(BeNil())
			edge := last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetAddEdge()
			Expect(edge.GetFrom()).To(Equal("#1"))
			Expect(edge.GetTo()).To(Equal("#2"))

			r = run(nil, "-p", "platform", "node", "show", "#1")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["node"]).To(HaveKeyWithValue("title", "Spike"))

			r = run(nil, "-p", "platform", "explain", "#2")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["reasons"]).To(HaveLen(1))

			r = run(nil, "-p", "platform", "node", "list", "--status", "open")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["nodes"]).To(HaveLen(2))
		})

		It("claim, heartbeat, release, complete, comment", func() {
			s.newNode("Work")
			r := run(nil, "-p", "platform", "claim", "--harness", "claude-code", "--agent-ref", "laptop")
			Expect(r.code).To(Equal(0), r.stderr.String())
			lease, _ := r.decode()["lease"].(map[string]any)
			Expect(lease).To(HaveKey("id"))
			id, _ := lease["id"].(string)
			sent := last[*daggerv1.ClaimNextRequest](s)
			Expect(sent.GetHarness()).To(Equal("claude-code"))
			Expect(sent.GetAgentRef()).To(Equal("laptop"))
			Expect(sent.GetWait().AsDuration()).To(BeZero())

			r = run(nil, "heartbeat", id, "--ttl", "10m")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["lease"]).To(HaveKeyWithValue("id", id))
			Expect(last[*daggerv1.HeartbeatRequest](s).GetTtl().AsDuration()).To(Equal(10 * time.Minute))

			r = run(nil, "release", id, "--reason", "crash", "--detail", "oom")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["lease"]).To(HaveKeyWithValue("release_reason", "RELEASE_REASON_CRASH"))

			r = run(nil, "release", id)
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--reason"))

			r = run(strings.NewReader("Shipped it.\n"), "-p", "platform", "complete", "#1", "--lease", id, "--result", "-")
			Expect(r.code).To(Equal(0), r.stderr.String())
			patch := last[*daggerv1.ApplyPatchRequest](s)
			Expect(patch.GetLeaseId()).To(Equal(id))
			Expect(patch.GetOps()[0].GetCompleteNode().GetResult().GetBody()).To(Equal("Shipped it.\n"))
			Expect(r.decode()["nodes"]).To(HaveLen(1))

			r = run(nil, "-p", "platform", "comment", "#1", "Looks good", "--in-reply-to", "01JCOMMENT0000000000000000")
			Expect(r.code).To(Equal(0), r.stderr.String())
			op := last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetAddComment()
			Expect(op.GetBody()).To(Equal("Looks good"))
			Expect(op.GetInReplyTo()).To(Equal("01JCOMMENT0000000000000000"))
		})

		It("search and events", func() {
			s.newNode("Parser spike")
			r := run(nil, "-p", "platform", "search", "parser", "--include-archived")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.decode()["hits"]).To(HaveLen(1))
			sent := last[*daggerv1.SearchRequest](s)
			Expect(sent.GetProject()).To(Equal("platform"))
			Expect(sent.GetIncludeArchived()).To(BeTrue())

			By("events stream as one JSON object per line until the stream ends")
			r = run(nil, "-p", "platform", "-o", "text", "events", "--cursor", "c0", "--op", "create_node")
			Expect(r.code).To(Equal(0), r.stderr.String())
			lines := strings.Split(strings.TrimSpace(r.stdout.String()), "\n")
			Expect(lines).To(HaveLen(2))
			for _, line := range lines {
				Expect(json.Valid([]byte(line))).To(BeTrue(), line)
			}
			Expect(lines[1]).To(ContainSubstring(`"cursor":"c2"`))
			Expect(last[*daggerv1.WatchEventsRequest](s).GetCursor()).To(Equal("c0"))
			Expect(last[*daggerv1.WatchEventsRequest](s).GetOps()).To(Equal([]string{"create_node"}))
		})
	})

	Describe("configuration", func() {
		It("reads the socket and project from the config file and the environment", func() {
			s.newNode("Configured")
			path := filepath.Join(GinkgoT().TempDir(), "config.json")
			Expect(os.WriteFile(path, []byte(`{"socket": "`+socket+`", "project": "from-file"}`), 0o600)).To(Succeed())

			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			code := cli.Run([]string{"--config", path, "ready"}, strings.NewReader(""), stdout, stderr)
			Expect(code).To(Equal(0), stderr.String())
			Expect(last[*daggerv1.ReadyRequest](s).GetProject()).To(Equal("from-file"))

			GinkgoT().Setenv(client.EnvSocket, socket)
			GinkgoT().Setenv(client.EnvProject, "from-env")
			stdout.Reset()
			code = cli.Run([]string{"ready"}, strings.NewReader(""), stdout, stderr)
			Expect(code).To(Equal(0), stderr.String())
			Expect(last[*daggerv1.ReadyRequest](s).GetProject()).To(Equal("from-env"))
		})
	})
})
