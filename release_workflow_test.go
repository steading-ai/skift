package skift_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// .github/workflows/release.yml turns a tag into a published release. The
// order of its jobs and what each may write are the security properties
// docs/adrs/011-releases-are-immutable-and-attested.md decides, so they
// are held here rather than left to review.
var _ = Describe("The release workflow", Label("unit"), func() {
	var w workflow

	BeforeEach(func() {
		w = loadWorkflow("release.yml")
	})

	It("is started by a pushed version tag and nothing else", func() {
		Expect(w.triggers()).To(ConsistOf("push"))
		Expect(w.On["push"].Tags).To(ConsistOf("v*"))
	})

	It("publishes nothing before it is signed", func() {
		Expect(w.Jobs["draft"].Needs).To(ContainElement("build"))
		Expect(w.Jobs["sign"].Needs).To(ContainElement("draft"))
		Expect(w.Jobs["publish"].Needs).To(ContainElement("sign"))
	})

	It("puts the compose file in the draft before it is signed", func() {
		Expect(w.Jobs["image"].Needs).To(ContainElement("build"))
		Expect(w.Jobs["compose"].Needs).To(ContainElement("image"))
		Expect(w.Jobs["draft"].Needs).To(ContainElements("image", "compose"))
		Expect(w.Jobs["sign"].Needs).To(ContainElement("draft"))
	})

	It("tags the image only after the release is published", func() {
		Expect(w.Jobs["tag-image"].Needs).To(ContainElements("publish", "image"))
		Expect(w.Jobs["verify"].Needs).To(ContainElement("tag-image"))
	})

	It("lets nothing wait on the notes", func() {
		Expect(w.Jobs["notes"].Needs).To(ContainElement("publish"))
		for name, job := range w.Jobs {
			Expect(job.Needs).NotTo(ContainElement("notes"), name)
		}
	})

	It("confines write access to the jobs that need it", func() {
		Expect(w.Permissions).To(BeEmpty(), "top-level permissions are empty; each job states its own")

		var releaseWriters, packageWriters []string
		for name, job := range w.Jobs {
			Expect(job.Permissions).NotTo(BeEmpty(), name)
			if job.Permissions["contents"] == "write" {
				releaseWriters = append(releaseWriters, name)
			}
			if job.Permissions["packages"] == "write" {
				packageWriters = append(packageWriters, name)
			}
		}
		// notes grants it to the workflow it calls, which edits the
		// release's title and notes; image, likewise, to the one that pushes.
		Expect(releaseWriters).To(ConsistOf("draft", "publish", "notes"))
		Expect(packageWriters).To(ConsistOf("image", "tag-image"))
	})

	It("lets only the tagging job, of those that run here, write packages", func() {
		for name, job := range w.Jobs {
			if job.Uses == "" && job.Permissions["packages"] == "write" {
				Expect(name).To(Equal("tag-image"))
			}
		}
	})

	It("lets the jobs that can write the release run nothing but gh", func() {
		for _, name := range []string{"draft", "publish"} {
			for _, step := range w.Jobs[name].Steps {
				if step.Uses != "" {
					Expect(step.Uses).To(HavePrefix("actions/download-artifact@"), name)
				}
			}
		}
	})

	It("passes nothing into a shell by interpolation", func() {
		for _, script := range w.scripts() {
			Expect(script).NotTo(ContainSubstring("${{"))
		}
	})

	It("builds from no cache another run could have written", func() {
		for name, job := range w.Jobs {
			for _, step := range job.Steps {
				Expect(step.Uses).NotTo(HavePrefix("actions/cache@"), name)
				Expect(step.Uses).NotTo(HavePrefix("actions/setup-go@"), name)
				if strings.HasPrefix(step.Uses, "jdx/mise-action@") {
					Expect(step.With).To(HaveKeyWithValue("cache", false), name)
				}
			}
		}
	})

	It("declares the skills and every SBOM in the manifest it signs", func() {
		var resources string
		for _, step := range w.Jobs["sign"].Steps {
			if strings.HasPrefix(step.Uses, "jdx/packslip@") {
				Expect(step.With).To(HaveKeyWithValue("upload", false), "the signing job cannot write the release")
				Expect(step.With).To(HaveKeyWithValue("attest", "link"), "the archives were attested where they were built")
				resources, _ = step.With["resources"].(string)
			}
		}

		declared := strings.Split(strings.TrimSpace(resources), "\n")
		Expect(declared).To(ContainElements(
			"skill/skift=repo:plugin/skift/skills/skift",
			"skill/epic=repo:plugin/skift/skills/epic",
		))
		for _, platform := range releasePlatforms {
			Expect(declared).To(ContainElement(SatisfyAll(
				HavePrefix("sbom/cyclonedx@"),
				HaveSuffix("_"+platform+".cdx.json"),
			)), platform)
		}
	})
})
