package skift_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// .github/workflows/verify-release.yml checks a published release with
// nothing but what the public can reach, so it must hold no power to
// change anything, and every check must name who it trusts.
var _ = Describe("The verify-release workflow", Label("unit"), func() {
	var w workflow

	BeforeEach(func() {
		w = loadWorkflow("verify-release.yml")
	})

	It("is called or dispatched with a tag, and started by nothing else", func() {
		Expect(w.triggers()).To(ConsistOf("workflow_call", "workflow_dispatch"))
		for name, trigger := range w.On {
			Expect(trigger.Inputs).To(HaveKey("tag"), name)
		}
	})

	It("holds no write permission", func() {
		Expect(w.Permissions).To(BeEmpty(), "top-level permissions are empty; each job states its own")
		for name, job := range w.Jobs {
			Expect(job.Permissions).NotTo(BeEmpty(), name)
			for scope, level := range job.Permissions {
				Expect(level).NotTo(Equal("write"), "%s: %s", name, scope)
			}
		}
	})

	It("passes nothing into a shell by interpolation", func() {
		for _, script := range w.scripts() {
			Expect(script).NotTo(ContainSubstring("${{"))
		}
	})

	It("pins the signer on every attestation check", func() {
		checks := 0
		for _, script := range w.scripts() {
			for _, command := range commands(script) {
				if strings.Contains(command, "gh attestation verify") {
					checks++
					Expect(command).To(ContainSubstring("--signer-workflow"), command)
				}
			}
		}
		Expect(checks).To(BeNumerically(">=", 2), "provenance and SBOM are each verified")
	})

	It("checks the image only for a release that carries one", func() {
		const carriesImage = "steps.image.outputs.released == 'true'"

		checks := 0
		for _, step := range w.Jobs["verify"].Steps {
			if strings.Contains(step.Run, "oci://") || strings.Contains(step.Run, "digest_of") {
				checks++
				Expect(step.If).To(Equal(carriesImage), step.Name)
			}
		}
		Expect(checks).To(Equal(2), "the image's attestations, and the digest the compose file pins")
	})

	It("installs every released platform with mise, by manifest and by asset name", func() {
		install := w.Jobs["install"]

		var platforms []string
		for _, leg := range install.Strategy.Matrix.Include {
			platforms = append(platforms, leg["platform"])
		}
		Expect(platforms).To(ConsistOf(releasePlatforms))

		var script strings.Builder
		for _, step := range install.Steps {
			script.WriteString(step.Run)
		}
		Expect(script.String()).To(ContainSubstring(`mise use "packslip:`))
		Expect(script.String()).To(ContainSubstring(`mise use "github:`))
		Expect(script.String()).To(ContainSubstring("mise skills ls"))
	})
})
