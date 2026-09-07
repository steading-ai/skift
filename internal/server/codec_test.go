package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/codec"
	"github.com/ivy/dagger/internal/testsupport"
)

// The handlers are mounted with dagger's JSON codec, so a client that
// speaks JSON is what these scenarios use: raw documents posted to the
// procedure, the way an agent writing JSON by hand reaches the one door.

// postJSON sends body to one procedure over the socket and returns the
// response document, or the Connect error the handler answered with.
func (f *fixture) postJSON(ctx context.Context, procedure, body string) (string, error) {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+procedure, strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	res, err := f.run.httpClient().Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer res.Body.Close() //nolint:errcheck // the response is fully read
	raw, err := io.ReadAll(res.Body)
	Expect(err).NotTo(HaveOccurred())
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s: %s", procedure, res.Status, raw)
	}
	return string(raw), nil
}

// createPatch is a patch on the platform project creating one node, with
// its priority and assignee kind spelt as given.
func createPatch(priority, assigneeKind string) string {
	return fmt.Sprintf(`{"request_id": %q, "project": "platform", "view": "full", "ops": [
		{"create_node": {"alias": "a", "title": "Ship it", "priority": %q, "assignee_kind": %q}}
	]}`, newID(), priority, assigneeKind)
}

var _ = Describe("Enums on the JSON wire", Label(testsupport.DBLabel), func() {
	var f *fixture
	const applyPatch = "/dagger.v1.GraphService/ApplyPatch"

	BeforeEach(func(ctx SpecContext) {
		f = newFixture(ctx)
		f.project(ctx, "Platform")
	})

	It("The short form is accepted on the wire", func(ctx SpecContext) {
		By(`given a patch with create_node priority "p1" and assignee_kind "user"`)
		patch := createPatch("p1", "user")

		By("when it is applied through the Connect handler")
		_, err := f.postJSON(ctx, applyPatch, patch)
		Expect(err).NotTo(HaveOccurred())

		By("then the node is created with priority P1 and assignee kind user")
		node := f.getNode(ctx, "platform", "#1").GetNode()
		Expect(node.GetPriority()).To(Equal(daggerv1.Priority_PRIORITY_P1))
		Expect(node.GetAssigneeKind()).To(Equal(daggerv1.AssigneeKind_ASSIGNEE_KIND_USER))
	})

	It("The canonical form is still accepted", func(ctx SpecContext) {
		By(`given the same patch with "PRIORITY_P1" and "ASSIGNEE_KIND_USER"`)
		short := createPatch("p1", "user")
		canonical := createPatch("PRIORITY_P1", "ASSIGNEE_KIND_USER")

		By("when it is applied")
		_, err := f.postJSON(ctx, applyPatch, short)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.postJSON(ctx, applyPatch, canonical)
		Expect(err).NotTo(HaveOccurred())

		By("then the result is identical")
		first := f.getNode(ctx, "platform", "#1").GetNode()
		second := f.getNode(ctx, "platform", "#2").GetNode()
		Expect(anonymize(second)).To(Equal(anonymize(first)))
	})

	It("Emission is short", func(ctx SpecContext) {
		By("given a node with priority P1")
		_, err := f.postJSON(ctx, applyPatch, createPatch("p1", "user"))
		Expect(err).NotTo(HaveOccurred())

		By("when it is read through any surface as JSON")
		overTheWire, err := f.postJSON(ctx, "/dagger.v1.GraphService/GetNode",
			`{"project": "platform", "node": "#1", "view": "full"}`)
		Expect(err).NotTo(HaveOccurred())
		res := f.getNode(ctx, "platform", "#1")
		encoded, err := codec.Marshal(res)
		Expect(err).NotTo(HaveOccurred())

		By(`then the priority field is the string "p1"`)
		for _, document := range []string{overTheWire, string(encoded)} {
			Expect(document).To(ContainSubstring(`"priority":"p1"`))
			Expect(document).NotTo(ContainSubstring("PRIORITY"))
			Expect(document).To(ContainSubstring(`"assignee_kind":"user"`))
			Expect(document).To(ContainSubstring(`"status":"open"`))
		}
	})

	It("answers a spelling that names no value with the values it takes", func(ctx SpecContext) {
		_, err := f.postJSON(ctx, applyPatch, createPatch("urgent", "user"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("urgent"))
	})
})

// anonymize renders a node without what makes it that node, so that two
// nodes built by the same patch differing only in spelling compare equal.
func anonymize(node *daggerv1.Node) string {
	GinkgoHelper()
	bare, ok := proto.Clone(node).(*daggerv1.Node)
	Expect(ok).To(BeTrue())
	bare.Id = ""
	bare.Number = 0
	bare.Reference = ""
	bare.CreatedAt = nil
	bare.ContextId = ""
	raw, err := codec.Marshal(bare)
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}
