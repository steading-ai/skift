package skift_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The workflows whose output is published run on GitHub's own runners:
// their builds read no cache another run could have written, and an
// attestation of a release says it came from a GitHub-hosted runner.
// docs/ci.md, "Runners", has the reasons; check.yml is the one that does
// not publish and runs elsewhere.
var _ = Describe("The workflows that publish", Label("unit"), func() {
	for _, name := range []string{"release.yml", "verify-release.yml", "release-notes.yml", "publish.yml"} {
		It("run "+name+" on GitHub-hosted runners", func() {
			for job, definition := range loadWorkflow(name).Jobs {
				Expect(fmt.Sprint(definition.RunsOn)).NotTo(ContainSubstring("blacksmith"), job)
			}
		})
	}
})
