package daggerv1

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// referenceFields are the request fields that name a node. Each is the one
// reference type: a string accepting #7, slug#7, org/slug#7, or an alias.
var referenceFields = map[string][]string{
	"CreateNodeOp":     {"parent", "blocked_by"},
	"UpdateNodeOp":     {"node", "parent"},
	"DropNodeOp":       {"node"},
	"CompleteNodeOp":   {"node"},
	"ReopenNodeOp":     {"node"},
	"AddEdgeOp":        {"from", "to"},
	"RemoveEdgeOp":     {"from", "to"},
	"AttachArtifactOp": {"node"},
	"AddCommentOp":     {"node"},
	"GetNodeRequest":   {"node"},
	"ExplainRequest":   {"node"},
	"ListNodesRequest": {"parent"},
}

// readOnly are the methods that write nothing and so need no idempotency
// key. Every other method is a mutation.
var readOnly = map[string]bool{
	"GetNode": true, "GetInputs": true, "ListNodes": true, "Explain": true, "Search": true,
	"GetProject": true, "ListProjects": true, "Ready": true, "WatchEvents": true,
}

// op is one operation of a patch as a client would write it: the message
// it is expressed with and the fields it sets.
type op struct {
	message string
	fields  map[string]string
}

// expectField asserts a message declares a field with the given type and
// cardinality.
func expectField(c contract, message, name, typ string, repeated bool) {
	GinkgoHelper()
	f := c.field(message, name)
	Expect(f.typ).To(Equal(typ), "%s.%s type", message, name)
	Expect(f.repeated).To(Equal(repeated), "%s.%s repeated", message, name)
}

// repoRoot is the module root, where buf.yaml lives.
func repoRoot() string {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	Expect(err).NotTo(HaveOccurred())
	return root
}

// buf runs a buf subcommand from the module root and returns its combined
// output, failing the spec if buf is not installed.
func buf(args ...string) (string, error) {
	cmd := exec.Command("buf", args...)
	cmd.Dir = repoRoot()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// emptyBaseline writes a buf module whose only file declares the dagger.v1
// package and nothing else, and returns its directory.
func emptyBaseline() string {
	dir := GinkgoT().TempDir()
	Expect(os.WriteFile(filepath.Join(dir, "buf.yaml"),
		[]byte("version: v2\nmodules:\n  - path: proto\n"), 0o644)).To(Succeed())
	pkg := filepath.Join(dir, "proto", "dagger", "v1")
	Expect(os.MkdirAll(pkg, 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(pkg, "dagger.proto"), []byte(strings.Join([]string{
		`syntax = "proto3";`,
		``,
		`package dagger.v1;`,
		``,
		`option go_package = "github.com/ivy/dagger/gen/dagger/v1;daggerv1";`,
		``,
	}, "\n")), 0o644)).To(Succeed())
	return dir
}

var _ = Describe("service.proto", func() {
	var c contract

	BeforeEach(func() {
		c = parse("service.proto")
		Expect(c.messages).NotTo(BeEmpty())
		Expect(c.services).NotTo(BeEmpty())
	})

	It("A decomposition is expressible in one call", func() {
		By("given a patch that creates two nodes and links them")
		patch := []op{
			{"CreateNodeOp", map[string]string{"alias": "design", "title": "Design the thing"}},
			{"CreateNodeOp", map[string]string{"alias": "build", "title": "Build the thing", "blocked_by": "design"}},
			{"AddEdgeOp", map[string]string{"from": "design", "to": "build"}},
		}

		By("when I inspect the request message")
		Expect(c.messages).To(HaveKey("ApplyPatchRequest"))
		expectField(c, "ApplyPatchRequest", "ops", "PatchOp", true)
		var alternatives []string
		for _, f := range c.fieldsOf("PatchOp") {
			alternatives = append(alternatives, f.typ)
		}
		for _, o := range patch {
			Expect(alternatives).To(ContainElement(o.message), "PatchOp cannot express %s", o.message)
			for name := range o.fields {
				Expect(c.field(o.message, name).typ).To(Equal("string"), "%s.%s", o.message, name)
			}
		}

		By("then the second node references the first by alias")
		Expect(c.field("CreateNodeOp", "alias").comment).To(ContainSubstring("usable as a node reference"))
		expectField(c, "CreateNodeOp", "blocked_by", "string", true)
		Expect(patch[1].fields["blocked_by"]).To(Equal(patch[0].fields["alias"]))
		Expect(patch[2].fields["from"]).To(Equal(patch[0].fields["alias"]))

		By("and no node id from the server is required")
		for _, o := range patch {
			for _, f := range c.fieldsOf(o.message) {
				Expect(f.name).NotTo(BeElementOf("id", "node_id", "number"), "%s.%s", o.message, f.name)
			}
		}
	})

	It("Claiming returns a fencing token", func() {
		By("when ClaimNext returns work")
		var claimNext rpc
		for _, r := range c.services["WorkService"] {
			if r.name == "ClaimNext" {
				claimNext = r
			}
		}
		Expect(claimNext.response).To(Equal("ClaimNextResponse"))
		expectField(c, "ClaimNextResponse", "node", "Node", false)

		By("then the response includes a lease id")
		expectField(c, "ClaimNextResponse", "lease", "Lease", false)
		domain := parse("dagger.proto")
		Expect(domain.field("Lease", "id").comment).To(ContainSubstring("fencing token"))

		By("and that lease id is required by ApplyPatch for that node")
		leaseID := c.field("ApplyPatchRequest", "lease_id")
		Expect(leaseID.typ).To(Equal("string"))
		Expect(leaseID.comment).To(And(
			ContainSubstring("fencing token"),
			ContainSubstring("Required for any operation that touches a leased node")))
	})

	It("passes buf lint and buf breaking against an empty baseline", func() {
		out, err := buf("lint")
		Expect(err).NotTo(HaveOccurred(), out)
		out, err = buf("breaking", "--against", emptyBaseline())
		Expect(err).NotTo(HaveOccurred(), out)
	})

	It("names every node reference with a single string type", func() {
		for message, names := range referenceFields {
			for _, name := range names {
				f := c.field(message, name)
				Expect(f.typ).To(Equal("string"), "%s.%s", message, name)
				Expect(f.comment).To(ContainSubstring("reference"), "%s.%s", message, name)
			}
		}
		Expect(c.messages).NotTo(HaveKey("NodeRef"), "a reference is a string, not a wrapper message")
	})

	It("filters ClaimNext by project, kind, assignee kind, labels, minimum priority, and wait", func() {
		expectField(c, "ClaimNextRequest", "project", "string", false)
		expectField(c, "ClaimNextRequest", "kind", "string", false)
		expectField(c, "ClaimNextRequest", "assignee_kind", "AssigneeKind", false)
		expectField(c, "ClaimNextRequest", "labels", "string", true)
		expectField(c, "ClaimNextRequest", "min_priority", "Priority", false)
		expectField(c, "ClaimNextRequest", "wait", "google.protobuf.Duration", false)
	})

	It("returns structured reasons from Explain, not prose", func() {
		reasons := c.field("ExplainResponse", "reasons")
		Expect(reasons.repeated).To(BeTrue())
		Expect(reasons.typ).To(Equal("Reason"))
		alternatives := c.fieldsOf("Reason")
		Expect(alternatives).NotTo(BeEmpty())
		for _, f := range alternatives {
			Expect(f.typ).NotTo(BeElementOf("string", "bytes"), "Reason.%s is prose", f.name)
			Expect(c.messages).To(HaveKey(f.typ), "Reason.%s", f.name)
		}
		for _, name := range []string{"blocked", "leased", "open_child"} {
			Expect(c.field("Reason", name)).NotTo(BeZero())
		}
	})

	It("carries an idempotency key on every mutating RPC", func() {
		rpcs := c.rpcs()
		Expect(rpcs).NotTo(BeEmpty())
		var mutating int
		for _, r := range rpcs {
			if readOnly[r.name] {
				continue
			}
			mutating++
			expectField(c, r.request, "request_id", "string", false)
		}
		Expect(mutating).To(BeNumerically(">", 0))
	})

	It("defines every service and RPC the work order names", func() {
		want := map[string][]string{
			"GraphService": {"ApplyPatch", "GetNode", "GetInputs", "ListNodes", "Explain", "Search",
				"CreateProject", "GetProject", "ListProjects", "UpdateProject", "ArchiveProject",
				"HoldProject", "UnholdProject", "Focus", "ClearFocus"},
			"WorkService":  {"Ready", "ClaimNext", "Heartbeat", "Release", "ReportUsage"},
			"EventService": {"WatchEvents"},
		}
		for service, names := range want {
			var got []string
			for _, r := range c.services[service] {
				got = append(got, r.name)
				Expect(r.request).To(Equal(r.name+"Request"), r.name)
				Expect(r.response).To(Equal(r.name+"Response"), r.name)
				Expect(c.messages).To(HaveKey(r.request))
				Expect(c.messages).To(HaveKey(r.response))
			}
			Expect(got).To(ConsistOf(names), service)
		}
	})

	It("streams WatchEvents with a resumable cursor", func() {
		var streamed []string
		for _, r := range c.services["EventService"] {
			if r.stream {
				streamed = append(streamed, r.name)
			}
		}
		Expect(streamed).To(ConsistOf("WatchEvents"))
		expectField(c, "WatchEventsResponse", "event", "Event", false)
		expectField(c, "WatchEventsResponse", "cursor", "string", false)
		expectField(c, "WatchEventsRequest", "cursor", "string", false)
	})

	It("expresses every patch operation the work order names", func() {
		var got []string
		for _, f := range c.fieldsOf("PatchOp") {
			got = append(got, f.name)
		}
		Expect(got).To(ConsistOf("create_node", "update_node", "drop_node", "complete_node",
			"reopen_node", "add_edge", "remove_edge", "attach_artifact", "add_comment",
			"edit_comment", "resolve_comment", "estimate_node", "hold_node", "unhold_node"))
		for _, name := range []string{"alias", "parent", "blocked_by"} {
			Expect(c.field("CreateNodeOp", name).typ).To(Equal("string"), name)
		}
	})

	It("Provenance cannot be asserted by a client", func() {
		settable := clientSettable(c)
		Expect(settable).NotTo(BeEmpty())
		for _, name := range []string{"principal", "principal_kind", "on_behalf_of",
			"identity_scheme", "holder", "author", "resolved_by"} {
			Expect(settable).NotTo(ContainElement(HaveSuffix("."+name)), "a request may set %s", name)
		}
	})

	It("comments every service, RPC, message, and field", func() {
		for _, r := range c.rpcs() {
			Expect(r.comment).NotTo(BeEmpty(), "%s.%s", r.service, r.name)
		}
		for name, comment := range c.messages {
			Expect(comment).NotTo(BeEmpty(), "message %s", name)
		}
		for _, f := range c.fields {
			Expect(f.comment).NotTo(BeEmpty(), "%s.%s", f.message, f.name)
		}
	})
})
