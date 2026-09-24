package acceptance_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/testsupport"
)

// An agent writes JSON by hand, and everything it has read says "p2". The
// canonical spelling keeps working for anything already written against
// the proto names.

// priorityPatch is a patch creating one node at the given spelling of P2.
func priorityPatch(priority string) string {
	return fmt.Sprintf(`{"project": %q, "ops": [
		{"create_node": {"alias": "a", "title": "Ship it", "priority": %q, "assignee_kind": "user"}}
	]}`, platform, priority)
}

// eventShape is one event without what makes it that event: its id, the
// ids and times of what it touched, and the request that caused it. What
// is left is what the patch asked for.
func eventShape(e *skiftv1.Event) string {
	after := e.GetAfter().AsMap()
	for _, key := range []string{"ID", "OrgID", "ProjectID", "Number", "Reference", "ContextID", "CreatedAt", "UpdatedAt"} {
		delete(after, key)
	}
	return fmt.Sprintf("%s %s %v", e.GetOp(), e.GetReason(), after)
}

func eventShapes(res *skiftv1.ApplyPatchResponse) []string {
	shapes := make([]string, 0, len(res.GetEvents()))
	for _, e := range res.GetEvents() {
		shapes = append(shapes, eventShape(e))
	}
	return shapes
}

var _ = Describe("Enum spellings", Label(testsupport.DBLabel), func() {
	It("takes the short and the canonical spelling of a priority, and prints the short one", func(ctx SpecContext) {
		d := deploy(ctx)
		d.laptop.createProject("Platform")

		By(`given the same patch spelt "p2" and spelt "PRIORITY_P2"`)
		short, canonical := priorityPatch("p2"), priorityPatch("PRIORITY_P2")

		By("when each is applied through skift patch apply")
		first := &skiftv1.ApplyPatchResponse{}
		d.laptop.callWithInput(first, short, "patch", "apply", "-", "--view", "full")
		second := &skiftv1.ApplyPatchResponse{}
		d.laptop.callWithInput(second, canonical, "patch", "apply", "-", "--view", "full")

		By("then both are created at P2, and the two patches produced the same events")
		Expect(first.GetNodes()[0].GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P2))
		Expect(second.GetNodes()[0].GetPriority()).To(Equal(skiftv1.Priority_PRIORITY_P2))
		Expect(first.GetNodes()[0].GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_USER))
		Expect(eventShapes(first)).NotTo(BeEmpty())
		Expect(eventShapes(second)).To(Equal(eventShapes(first)))

		By(`and skift node show prints "priority": "p2"`)
		stdout, stderr, code := d.laptop.run("", "node", "show", "--project", platform, ref(first.GetNodes()[0]))
		Expect(code).To(BeZero(), "skift node show failed: %s", stderr)
		Expect(stdout).To(ContainSubstring(`"priority":"p2"`))
		Expect(stdout).NotTo(ContainSubstring("PRIORITY_"))
	})
})
