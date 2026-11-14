package skift_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// .github/workflows/release-image.yml pushes a release's image and attests
// it. That it can only be called, pushes under no tag, and builds from the
// binaries it is handed are what
// docs/adrs/011-releases-are-immutable-and-attested.md decides for the
// image, so they are held here: nothing can run the workflow before a
// release does.
var _ = Describe("The release image workflow", Label("unit"), func() {
	const (
		image  = "ghcr.io/steading-ai/skift"
		digest = "${{ steps.push.outputs.digest }}"
	)

	var w workflow

	BeforeEach(func() {
		w = loadWorkflow("release-image.yml")
	})

	It("The image workflow can only be called", func() {
		By("given .github/workflows/release-image.yml, when its triggers are read")
		triggers := w.triggers()

		By("then workflow_call is the only one")
		Expect(triggers).To(ConsistOf("workflow_call"))

		By("and it declares inputs named version and binaries and an output named digest")
		call := w.On["workflow_call"]
		Expect(call.Inputs).To(HaveLen(2))
		for _, name := range []string{"version", "binaries"} {
			Expect(call.Inputs).To(HaveKeyWithValue(name, SatisfyAll(
				HaveKeyWithValue("required", true),
				HaveKeyWithValue("type", "string"),
			)), name)
		}
		Expect(call.Outputs).To(HaveLen(1))
		Expect(call.Outputs).To(HaveKeyWithValue("digest",
			HaveKeyWithValue("value", "${{ jobs.image.outputs.digest }}")))
		Expect(w.Jobs["image"].Outputs).To(HaveKeyWithValue("digest", digest))
	})

	It("The image workflow never tags and never compiles", func() {
		By("given .github/workflows/release-image.yml, when every step is read")
		var steps []workflowStep
		for _, job := range w.Jobs {
			steps = append(steps, job.Steps...)
		}
		Expect(steps).NotTo(BeEmpty())

		By("then none sets tags")
		for _, step := range steps {
			Expect(step.With).NotTo(HaveKey("tags"), step.text())
		}

		By("and none names ghcr.io/steading-ai/skift followed by a colon")
		for _, step := range steps {
			Expect(step.text()).NotTo(ContainSubstring(image + ":"))
		}
		Expect(w.Env).NotTo(ContainElement(ContainSubstring(image+":")), "nor does the environment every step shares")

		By("and none runs go build")
		for _, step := range steps {
			Expect(step.Uses).NotTo(HavePrefix("actions/setup-go@"))
			for _, command := range commands(step.Run) {
				Expect(command).NotTo(MatchRegexp(`\bgo\s+build\b`))
				Expect(command).NotTo(MatchRegexp(`\bmake\b.*\bdist/`))
			}
		}
	})

	It("holds only what pushing and attesting an image take, in its one job", func() {
		Expect(w.Permissions).To(BeEmpty(), "top-level permissions are empty; the job states its own")
		Expect(w.Jobs).To(HaveLen(1))
		Expect(w.Jobs["image"].Permissions).To(Equal(map[string]string{
			"contents":     "read",
			"packages":     "write",
			"id-token":     "write",
			"attestations": "write",
		}))
	})

	It("passes nothing into a shell by interpolation", func() {
		for _, script := range w.scripts() {
			Expect(script).NotTo(ContainSubstring("${{"))
		}
	})

	It("builds from the binaries it is handed, where the Containerfile reads them", func() {
		downloads := w.steps("actions/download-artifact")
		Expect(downloads).To(HaveLen(1))
		Expect(downloads[0].With).To(HaveKeyWithValue("name", "${{ inputs.binaries }}"))
		Expect(downloads[0].With).To(HaveKeyWithValue("path", "dist"))

		for _, checkout := range w.steps("actions/checkout") {
			Expect(checkout.With).To(HaveKeyWithValue("persist-credentials", false))
		}
	})

	It("pushes both platforms by digest, labelled with the version and the licence", func() {
		Expect(w.Env).To(HaveKeyWithValue("IMAGE", image))

		builds := w.steps("docker/build-push-action")
		Expect(builds).To(HaveLen(1))
		build := builds[0]
		Expect(build.ID).To(Equal("push"))
		Expect(build.With).To(HaveKeyWithValue("file", "packaging/Containerfile"))
		Expect(build.With).To(HaveKeyWithValue("platforms", "linux/amd64,linux/arm64"))
		Expect(build.With).To(HaveKeyWithValue("outputs",
			"type=image,name=${{ env.IMAGE }},push-by-digest=true,name-canonical=true,push=true"))
		Expect(build.With).NotTo(HaveKey("push"), "the push shorthand pushes under a name, which needs a tag")
		Expect(build.With).To(HaveKeyWithValue("build-args", ContainSubstring("REVISION=${{ github.sha }}")))
		Expect(build.With).To(HaveKeyWithValue("labels", SatisfyAll(
			ContainSubstring("org.opencontainers.image.version=${{ inputs.version }}"),
			ContainSubstring("org.opencontainers.image.licenses=ISC"),
		)))
	})

	It("attests the index twice, its provenance and its SBOM, and pushes both to the registry", func() {
		attestations := w.steps("actions/attest")
		Expect(attestations).To(HaveLen(2))

		var sboms []any
		for _, attest := range attestations {
			Expect(attest.With).To(HaveKeyWithValue("subject-name", "${{ env.IMAGE }}"))
			Expect(attest.With).To(HaveKeyWithValue("subject-digest", digest))
			Expect(attest.With).To(HaveKeyWithValue("push-to-registry", true))
			if sbom, ok := attest.With["sbom-path"]; ok {
				sboms = append(sboms, sbom)
			}
		}
		Expect(sboms).To(ConsistOf("image.cdx.json"), "one attestation is the SBOM's, the other the provenance")

		By("cataloguing the pushed image with the syft mise.toml pins, and no other tool")
		tools := w.steps("jdx/mise-action")
		Expect(tools).To(HaveLen(1))
		Expect(tools[0].With).To(HaveKeyWithValue("install_args", "syft"))
		Expect(tools[0].With).To(HaveKeyWithValue("cache", false))
		Expect(w.scripts()).To(ContainElement(SatisfyAll(
			ContainSubstring("syft scan"),
			ContainSubstring(`"registry:$IMAGE@$DIGEST"`),
			ContainSubstring("cyclonedx-json=image.cdx.json"),
		)))
	})
})
