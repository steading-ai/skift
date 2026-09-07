package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/cli"
	"github.com/ivy/dagger/internal/client"
	"github.com/ivy/dagger/internal/codec"
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
			Expect(codec.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
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

	// A decode failure is the caller's next prompt, so it names the
	// operation at fault and what that field will take. The legal values
	// and the legal fields come from the descriptor here as they do in the
	// code, so neither side of the spec is a table that can go stale.
	Describe("Scenario: An enum error teaches the values", func() {
		It("An enum error teaches the values", func() {
			By(`given a patch whose second op has priority "high"`)
			patch := strings.NewReader(`{"ops": [
			  {"create_node": {"title": "First"}},
			  {"create_node": {"title": "Second", "priority": "high"}}
			]}`)

			By("when it is applied")
			r := run(patch, "-p", "platform", "patch", "apply", "-")

			By("then the error names ops[1], the field priority, and lists p0 through p4")
			Expect(r.code).To(Equal(cli.ExitUsage))
			msg := r.stderrError().Message
			Expect(msg).To(ContainSubstring("ops[1]"))
			Expect(msg).To(ContainSubstring("priority"))
			legal := codec.EnumNames(daggerv1.Priority(0).Descriptor())
			Expect(legal).NotTo(BeEmpty())
			Expect(msg).To(ContainSubstring(strings.Join(legal, ", ")))
			for _, value := range legal {
				Expect(msg).To(ContainSubstring(value))
			}
		})

		It("names the operation at fault in a long patch", func() {
			ops := make([]string, 28)
			for i := range ops {
				ops[i] = fmt.Sprintf(`{"update_node": {"node": "#%d", "body": "rewritten"}}`, i+1)
			}
			ops[3] = `{"update_node": {"node": "#4", "priority": "urgent"}}`
			r := run(strings.NewReader(`{"ops": [`+strings.Join(ops, ",")+`]}`), "-p", "platform", "patch", "apply", "-")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(HavePrefix("ops[3].update_node.priority: "))
		})
	})

	Describe("Scenario: An unknown field error teaches the fields", func() {
		It("An unknown field error teaches the fields", func() {
			By(`given a patch whose op uses "ref" where update_node expects "node"`)
			patch := strings.NewReader(`{"ops": [{"update_node": {"ref": "#1", "title": "Renamed"}}]}`)

			By("when it is applied")
			r := run(patch, "-p", "platform", "patch", "apply", "-")

			By(`then the error names "ref" as unknown and lists node, title, body and priority among the legal fields`)
			Expect(r.code).To(Equal(cli.ExitUsage))
			msg := r.stderrError().Message
			Expect(msg).To(ContainSubstring("ops[0].update_node"))
			Expect(msg).To(ContainSubstring(`unknown field "ref"`))
			fields := (&daggerv1.UpdateNodeOp{}).ProtoReflect().Descriptor().Fields()
			_, listed, found := strings.Cut(msg, "fields are ")
			Expect(found).To(BeTrue(), "the error lists the legal fields: %s", msg)
			for _, name := range []string{"node", "title", "body", "priority"} {
				Expect(fields.ByName(protoreflect.Name(name))).NotTo(BeNil(), "%s is a field of UpdateNodeOp", name)
				Expect(listed).To(ContainSubstring(name))
			}
		})

		It("says what a mistyped field takes", func() {
			r := run(strings.NewReader(`{"ops": [{"create_node": {"title": "A", "blocked_by": "#1"}}]}`), "-p", "platform", "patch", "apply", "-")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(Equal("ops[0].create_node.blocked_by: expected an array of strings"))
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
			Expect(codec.Unmarshal(raw, res)).To(Succeed())
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

	Describe("Scenario: Each command asks for the view its output needs", func() {
		BeforeEach(func() { s.newNode("Viewed") })

		It("defaults reads to a summary, node show to full, and writes to refs", func() {
			Expect(run(nil, "-p", "platform", "ready").code).To(Equal(0))
			Expect(last[*daggerv1.ReadyRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))

			Expect(run(nil, "-p", "platform", "node", "list").code).To(Equal(0))
			Expect(last[*daggerv1.ListNodesRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))

			Expect(run(nil, "-p", "platform", "search", "viewed").code).To(Equal(0))
			Expect(last[*daggerv1.SearchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))

			Expect(run(nil, "-p", "platform", "node", "show", "#1").code).To(Equal(0))
			Expect(last[*daggerv1.GetNodeRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_FULL))

			Expect(run(strings.NewReader(`{"ops": [{"create_node": {"title": "A", "body": "long"}}]}`), "-p", "platform", "patch", "apply", "-").code).To(Equal(0))
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_REFS))

			for _, args := range [][]string{
				{"create", "B", "--body", "long"},
				{"link", "#1", "#2"},
				{"comment", "#1", "a note"},
				{"complete", "#1", "--result", "done"},
			} {
				r := run(nil, append([]string{"-p", "platform"}, args...)...)
				Expect(r.code).To(Equal(0), r.stderr.String())
				Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_REFS), strings.Join(args, " "))
			}
		})

		It("--view overrides the default and rejects a view that is not one", func() {
			Expect(run(nil, "-p", "platform", "ready", "--view", "full").code).To(Equal(0))
			Expect(last[*daggerv1.ReadyRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_FULL))

			Expect(run(nil, "-p", "platform", "node", "show", "#1", "--view", "refs").code).To(Equal(0))
			Expect(last[*daggerv1.GetNodeRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_REFS))

			Expect(run(strings.NewReader(`{"ops": [{"create_node": {"title": "A"}}]}`), "-p", "platform", "patch", "apply", "-", "--view", "full").code).To(Equal(0))
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_FULL))

			By("a view the patch file names is kept unless the flag overrides it")
			Expect(run(strings.NewReader(`{"view": "VIEW_SUMMARY", "ops": [{"create_node": {"title": "A"}}]}`), "-p", "platform", "patch", "apply", "-").code).To(Equal(0))
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))
			Expect(run(strings.NewReader(`{"view": "VIEW_SUMMARY", "ops": [{"create_node": {"title": "A"}}]}`), "-p", "platform", "patch", "apply", "-", "--view", "refs").code).To(Equal(0))
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_REFS))

			r := run(nil, "-p", "platform", "ready", "--view", "huge")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring(`--view: "huge" is not one of full, refs, summary`))
		})

		It("renders a refs response as numbers, states, and references in text mode", func() {
			s.refsOnly = true
			r := run(strings.NewReader(`{"ops": [{"create_node": {"alias": "a", "title": "A"}}]}`), "-p", "platform", "-o", "text", "patch", "apply", "-")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(r.stdout.String()).To(ContainSubstring("#2     ready    platform#2"))
			Expect(r.stdout.String()).To(ContainSubstring("alias a = platform#2"))
			Expect(r.stdout.String()).To(ContainSubstring("1 events, request "))
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

	Describe("dg drop", func() {
		BeforeEach(func() { s.newNode("Target") })

		It("builds the same patch as the hand-written JSON", func() {
			By(`when I run "dg drop dagger#7 --reason superseded"`)
			r := run(nil, "drop", "dagger#7", "--reason", "superseded")
			Expect(r.code).To(Equal(0), r.stderr.String())
			built := last[*daggerv1.ApplyPatchRequest](s)

			By("then the operation is the one the same patch written by hand sends")
			r = run(strings.NewReader(`{"project": "dagger", "ops": [{"drop_node": {"node": "dagger#7", "reason": "superseded"}}]}`), "patch", "apply", "-")
			Expect(r.code).To(Equal(0), r.stderr.String())
			written := last[*daggerv1.ApplyPatchRequest](s)
			Expect(built.GetProject()).To(Equal(written.GetProject()))
			Expect(proto.Equal(built.GetOps()[0], written.GetOps()[0])).To(BeTrue(), "built %v, written %v", built.GetOps()[0], written.GetOps()[0])

			By("and the response lists the reference that was dropped")
			Expect(r.decode()["dropped"]).To(HaveLen(1))
		})

		It("carries the detail and the reason as the operation's fields", func() {
			r := run(nil, "-p", "platform", "drop", "#7", "--reason", "other", "--detail", "folded into #9")
			Expect(r.code).To(Equal(0), r.stderr.String())
			op := last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetDropNode()
			Expect(op.GetNode()).To(Equal("#7"))
			Expect(op.GetReason()).To(Equal(daggerv1.DropReason_DROP_REASON_OTHER))
			Expect(op.GetDetail()).To(Equal("folded into #9"))
			Expect(op.GetCascade()).To(BeNil())
		})

		It("--cascade N states the bound and no --cascade cascades nothing", func() {
			r := run(nil, "-p", "platform", "drop", "#7", "--reason", "superseded", "--cascade", "5")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetDropNode().GetCascade().GetMaxNodes()).To(BeEquivalentTo(5))

			r = run(nil, "-p", "platform", "drop", "#7", "--reason", "superseded")
			Expect(r.code).To(Equal(0), r.stderr.String())
			Expect(last[*daggerv1.ApplyPatchRequest](s).GetOps()[0].GetDropNode().GetCascade()).To(BeNil())
		})

		It("refuses --cascade without a number, and a bound below one", func() {
			r := run(nil, "-p", "platform", "drop", "#7", "--reason", "superseded", "--cascade")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("cascade"))

			r = run(nil, "-p", "platform", "drop", "#7", "--reason", "superseded", "--cascade", "0")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--cascade"))
		})

		It("refuses a drop with no reason, before any round trip", func() {
			r := run(nil, "-p", "platform", "drop", "#7")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--reason"))
			Expect(s.requests).To(BeEmpty())
		})
	})

	Describe("dg update", func() {
		BeforeEach(func() { s.newNode("Target") })

		update := func(stdin io.Reader, args ...string) *daggerv1.UpdateNodeOp {
			GinkgoHelper()
			r := run(stdin, args...)
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetOps()).To(HaveLen(1))
			op := sent.GetOps()[0].GetUpdateNode()
			Expect(op).NotTo(BeNil())
			return op
		}

		It("reprioritises with one update_node op and changes nothing else", func() {
			By(`when I run "dg update dagger#7 --priority p0"`)
			op := update(nil, "update", "dagger#7", "--priority", "p0")

			By("then the op sets priority P0 on that node")
			Expect(op.GetNode()).To(Equal("dagger#7"))
			Expect(op.Priority).NotTo(BeNil())
			Expect(op.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P0))

			By("and every other field is unset, so the server leaves it alone")
			Expect(op.Title).To(BeNil())
			Expect(op.Body).To(BeNil())
			Expect(op.Kind).To(BeNil())
			Expect(op.Parent).To(BeNil())
			Expect(op.Assignee).To(BeNil())
			Expect(op.AssigneeKind).To(BeNil())
			Expect(op.Labels).To(BeNil())

			By("and the patch is the one the same JSON written by hand sends")
			built := last[*daggerv1.ApplyPatchRequest](s)
			r := run(strings.NewReader(`{"project": "dagger", "ops": [{"update_node": {"node": "dagger#7", "priority": "p0"}}]}`), "patch", "apply", "-")
			Expect(r.code).To(Equal(0), r.stderr.String())
			written := last[*daggerv1.ApplyPatchRequest](s)
			Expect(built.GetProject()).To(Equal(written.GetProject()))
			Expect(proto.Equal(built.GetOps()[0], written.GetOps()[0])).To(BeTrue(), "built %v, written %v", built.GetOps()[0], written.GetOps()[0])
		})

		It("reparents, and --parent '' clears the parent", func() {
			By(`when I run "dg update dagger#7 --parent dagger#3"`)
			op := update(nil, "update", "dagger#7", "--parent", "dagger#3")

			By("then the op carries the new parent")
			Expect(op.Parent).NotTo(BeNil())
			Expect(op.GetParent()).To(Equal("dagger#3"))

			By(`when I run "dg update dagger#7 --parent ''"`)
			op = update(nil, "update", "dagger#7", "--parent", "")

			By("then the parent is present and empty, which makes the node a root")
			Expect(op.Parent).NotTo(BeNil())
			Expect(op.GetParent()).To(BeEmpty())

			By("and with no --parent at all the field is unset")
			op = update(nil, "update", "dagger#7", "--title", "Renamed")
			Expect(op.Parent).To(BeNil())
		})

		It("refuses a malformed parent reference before any round trip", func() {
			r := run(nil, "-p", "platform", "update", "#7", "--parent", "not a ref")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(s.requests).To(BeEmpty())
		})

		It("reads the body from stdin for --body -", func() {
			By(`when I run "dg update dagger#7 --body -" with a body on stdin`)
			op := update(strings.NewReader("# Rewritten\n\nthe new prompt\n"), "update", "dagger#7", "--body", "-")

			By("then the op carries what stdin held")
			Expect(op.Body).NotTo(BeNil())
			Expect(op.GetBody()).To(Equal("# Rewritten\n\nthe new prompt\n"))
		})

		It("replaces the label set, and --label '' clears it", func() {
			op := update(nil, "update", "dagger#7", "--label", "api", "--label", "backend")
			Expect(op.GetLabels().GetValues()).To(ConsistOf("api", "backend"))

			op = update(nil, "update", "dagger#7", "--label", "")
			Expect(op.Labels).NotTo(BeNil())
			Expect(op.GetLabels().GetValues()).To(BeEmpty())
		})

		It("carries the title, kind, assignee and assignee kind, and the lease", func() {
			r := run(nil, "update", "dagger#7", "--title", "Renamed", "--kind", "bug",
				"--assignee-kind", "user", "--assignee", "01JPRINCIPAL0000000000000A", "--lease", "01JLEASE0000000000000000001")
			Expect(r.code).To(Equal(0), r.stderr.String())
			sent := last[*daggerv1.ApplyPatchRequest](s)
			Expect(sent.GetLeaseId()).To(Equal("01JLEASE0000000000000000001"))
			op := sent.GetOps()[0].GetUpdateNode()
			Expect(op.GetTitle()).To(Equal("Renamed"))
			Expect(op.GetKind()).To(Equal("bug"))
			Expect(op.GetAssigneeKind()).To(Equal(daggerv1.AssigneeKind_ASSIGNEE_KIND_USER))
			Expect(op.GetAssignee()).To(Equal("01JPRINCIPAL0000000000000A"))
		})

		It("is a usage error with no field flags, naming them", func() {
			r := run(nil, "-p", "platform", "update", "#7")
			Expect(r.code).To(Equal(cli.ExitUsage))
			for _, flag := range []string{"--title", "--body", "--kind", "--priority", "--parent", "--label", "--assignee-kind", "--assignee"} {
				Expect(r.stderrError().Message).To(ContainSubstring(flag))
			}
			Expect(s.requests).To(BeEmpty())
		})

		It("teaches the values when a flag it was given takes an enum", func() {
			r := run(nil, "-p", "platform", "update", "#7", "--priority", "urgent")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("p0"))

			r = run(nil, "-p", "platform", "update", "#7", "--priority", "")
			Expect(r.code).To(Equal(cli.ExitUsage))
			Expect(r.stderrError().Message).To(ContainSubstring("--priority"))
		})

		It("has exactly one flag per field of UpdateNodeOp", func() {
			cmd, _, err := cli.NewRootCommand(io.Discard, io.Discard).Find([]string{"update"})
			Expect(err).NotTo(HaveOccurred())
			Expect(cmd.Name()).To(Equal("update"))

			By("then every field of the op has the flag that sets it")
			// node is the positional argument; labels is repeatable, so its
			// flag is singular. Every other field is its own name in kebab.
			flagNames := map[string]string{"node": "", "labels": "label"}
			fields := (&daggerv1.UpdateNodeOp{}).ProtoReflect().Descriptor().Fields()
			isField := map[string]bool{}
			for i := range fields.Len() {
				fd := fields.Get(i)
				name, ok := flagNames[string(fd.Name())]
				if !ok {
					name = strings.ReplaceAll(string(fd.Name()), "_", "-")
				}
				if name == "" {
					continue
				}
				Expect(cmd.Flags().Lookup(name)).NotTo(BeNil(), "no --%s flag for UpdateNodeOp field %s", name, fd.Name())
				isField[name] = true
			}

			By("and no flag beyond them, other than the ones every write takes")
			var rest []string
			cmd.Flags().VisitAll(func(f *pflag.Flag) {
				if !isField[f.Name] {
					rest = append(rest, f.Name)
				}
			})
			Expect(rest).To(ConsistOf("lease", "request-id", "view"))
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
			Expect(codec.Unmarshal(r.stdout.Bytes(), res)).To(Succeed())
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
			Expect(r.decode()["lease"]).To(HaveKeyWithValue("release_reason", "crash"))

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

	Describe("dg inputs", func() {
		It("prints the same JSON the RPC returned, and asks for the full view by default", func() {
			s.newNode("Alpha")
			s.newNode("Beta")
			s.newNode("Gamma")

			By(`when I run "dg inputs 'platform#3'"`)
			r := run(nil, "inputs", "platform#3")
			Expect(r.code).To(Equal(0), r.stderr.String())

			By("then the reference is sent as written, in the full view")
			req := last[*daggerv1.GetInputsRequest](s)
			Expect(req.GetNode()).To(Equal("platform#3"))
			Expect(req.GetView()).To(Equal(daggerv1.View_VIEW_FULL))

			By("and stdout is the response the RPC gives for the same request")
			c, err := client.New(client.Config{Socket: socket})
			Expect(err).NotTo(HaveOccurred())
			res, err := c.Graph.GetInputs(GinkgoT().Context(), connect.NewRequest(&daggerv1.GetInputsRequest{Node: "platform#3"}))
			Expect(err).NotTo(HaveOccurred())
			expected, err := codec.Marshal(res.Msg)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(r.stdout.String())).To(Equal(string(expected)))
			Expect(res.Msg.GetInputs()).To(HaveLen(2))
			Expect(r.stdout.String()).To(ContainSubstring("result of Alpha"))

			By("and --view summary is sent as asked")
			Expect(run(nil, "inputs", "platform#3", "--view", "summary").code).To(Equal(0))
			Expect(last[*daggerv1.GetInputsRequest](s).GetView()).To(Equal(daggerv1.View_VIEW_SUMMARY))

			By("and a bare reference needs a project, as every other command does")
			Expect(run(nil, "inputs", "#3").code).To(Equal(cli.ExitUsage))
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
