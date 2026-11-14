package skift_test

import (
	"os"
	"path"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ciDocument is the page that says what each workflow does.
const ciDocument = "docs/ci.md"

// workflowsDir holds the workflows, each a .yml file.
const workflowsDir = ".github/workflows/"

// isWorkflow reports whether a path from the repository root names a
// workflow file.
func isWorkflow(name string) bool {
	return strings.HasPrefix(name, workflowsDir) && strings.HasSuffix(name, ".yml")
}

var _ = Describe("The CI document", Label("unit"), func() {
	It("The CI document lists every workflow", func() {
		By("given the files under .github/workflows ending in .yml")
		var workflows []string
		for _, name := range trackedFiles() {
			if isWorkflow(name) {
				workflows = append(workflows, name)
			}
		}
		Expect(workflows).To(ContainElement(workflowsDir + "check.yml"))

		By("when docs/ci.md is read")
		raw, err := os.ReadFile(ciDocument)
		Expect(err).NotTo(HaveOccurred())
		links, _ := scanMarkdown(string(raw))
		var linked []string
		for _, link := range links {
			if hasScheme.MatchString(link.dest) {
				continue
			}
			target, _, _ := strings.Cut(link.dest, "#")
			file := path.Join(path.Dir(ciDocument), target)
			if isWorkflow(file) && !slices.Contains(linked, file) {
				linked = append(linked, file)
			}
		}

		By("then each file is linked from it")
		for _, name := range workflows {
			Expect(linked).To(ContainElement(name), "%s has no entry in %s", name, ciDocument)
		}

		By("and it links no workflow file that does not exist")
		for _, name := range linked {
			Expect(workflows).To(ContainElement(name), "%s links %s, which is not a workflow in the repository", ciDocument, name)
		}
	})
})
