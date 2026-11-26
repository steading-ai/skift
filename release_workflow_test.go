package skift_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
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

	It("refuses a draft missing any platform's archive", func() {
		By("given the platforms a release is built for")
		By("then the draft's completeness check expects one archive for each of them")
		var check string
		for _, s := range w.Jobs["draft"].Steps {
			if strings.HasPrefix(s.Name, "Every archive has its SBOM") {
				check = s.Run
			}
		}
		Expect(check).NotTo(BeEmpty(), "the draft job has no completeness check")
		Expect(check).To(ContainSubstring(fmt.Sprintf(`test "$archives" -eq %d`, len(releasePlatforms))))
	})

	// pkgs.stead.ing signs an rpm only once it has checked the rpm's
	// attestation, as the draft's check does here.
	It("attests the rpms, and refuses a draft without one for each architecture", func() {
		var attested []string
		for _, step := range w.Jobs["build"].Steps {
			if strings.HasPrefix(step.Uses, "actions/attest@") {
				subject, _ := step.With["subject-path"].(string)
				attested = append(attested, subject)
			}
		}
		Expect(attested).To(ContainElement("dist/release/*.rpm"))

		var check string
		for _, s := range w.Jobs["draft"].Steps {
			if strings.HasPrefix(s.Name, "Each architecture has its rpm") {
				check = s.Run
			}
		}
		Expect(check).NotTo(BeEmpty(), "the draft job has no check of the rpms")
		Expect(check).To(ContainSubstring(`want="skift-$VERSION-1.aarch64.rpm skift-$VERSION-1.x86_64.rpm"`))
		Expect(check).To(ContainSubstring(`grep -q "  $rpm\$" SHA256SUMS`))
		Expect(check).To(ContainSubstring("--deny-self-hosted-runners"))
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

	// docs/adrs/017-release-channels-and-packages.md, decision 3: a release
	// moves edge, and promotion moves the rest.
	It("moves no tag that promotion owns, and marks nothing a prerelease", func() {
		for _, script := range w.scripts() {
			Expect(script).NotTo(MatchRegexp(`\b(latest|stable)\b`))
			Expect(script).NotTo(ContainSubstring("--prerelease"))
		}
	})

	Describe("the version step", Label("slow"), func() {
		var dir string

		// The build job's step that reads the version, and where it falls
		// among the job's steps.
		versionStep := func() (workflowStep, int) {
			GinkgoHelper()
			for i, step := range w.Jobs["build"].Steps {
				if step.ID == "version" {
					return step, i
				}
			}
			Fail("the build job has no step with id version")
			return workflowStep{}, -1
		}

		// runVersionStep runs the step's script in the copy as the runner
		// would, for a push of tag, and returns the session and what the
		// step handed to later steps.
		runVersionStep := func(tag string) (*gexec.Session, string) {
			GinkgoHelper()
			step, _ := versionStep()
			output := filepath.Join(GinkgoT().TempDir(), "github_output")
			Expect(os.WriteFile(output, nil, 0o600)).To(Succeed())

			// https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idstepsshell
			cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", step.Run)
			cmd.Dir = dir
			for _, kv := range os.Environ() {
				if inherited(kv) {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "TAG="+tag, "GITHUB_OUTPUT="+output)
			session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			session.Wait(time.Minute)

			handed, err := os.ReadFile(output)
			Expect(err).NotTo(HaveOccurred())
			return session, string(handed)
		}

		// The copy has no history of its own, so each spec gives it one
		// commit, clean, for its tag to sit on.
		BeforeEach(func() {
			dir = checkout()
			git(dir, "init", "-q")
			git(dir, "add", "-A")
			git(dir, "commit", "-q", "--no-verify", "-m", "initial")
		})

		It("A tag with a pre-release part fails the build before anything is built", func() {
			By("given a checkout tagged v0.0.0-rc.1")
			git(dir, "tag", "v0.0.0-rc.1")

			By("when the build job's version step runs for that tag")
			session, handed := runVersionStep("v0.0.0-rc.1")

			By("then it exits non-zero")
			Expect(session).NotTo(gexec.Exit(0))
			By("and its error names ADR-017")
			Expect(string(session.Out.Contents())).To(SatisfyAll(
				ContainSubstring("::error::"),
				ContainSubstring("ADR-017"),
			))
			By("and it hands no version on")
			Expect(handed).To(BeEmpty())
			By("and no step but the checkout runs before it")
			_, at := versionStep()
			for _, step := range w.Jobs["build"].Steps[:at] {
				Expect(step.Uses).To(HavePrefix("actions/checkout@"), step.Name)
			}
		})

		It("passes a plain version tag and hands its version on", func() {
			git(dir, "tag", "v9.9.9")
			session, handed := runVersionStep("v9.9.9")
			Expect(session).To(gexec.Exit(0))
			Expect(handed).To(Equal("version=9.9.9\n"))
		})
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
			Expect(job.Permissions).NotTo(BeNil(), "%s states its own permissions, even when that is none", name)
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

	// docs/adrs/017-release-channels-and-packages.md: pkgs.stead.ing signs
	// what it has verified, so the dispatch only says "look now".
	It("starts pkgs.stead.ing's publish once the release is published, holding nothing of this repository", func() {
		job := w.Jobs["dispatch"]
		Expect(job.Needs).To(ConsistOf("publish"))
		Expect(job.Permissions).NotTo(BeNil())
		Expect(job.Permissions).To(BeEmpty(), "the App's token does the work; the job needs no GITHUB_TOKEN permission")

		Expect(job.Steps).To(HaveLen(2))
		mint, start := job.Steps[0], job.Steps[1]
		Expect(mint.Uses).To(HavePrefix("actions/create-github-app-token@"))
		Expect(mint.With).To(HaveKeyWithValue("repositories", "pkgs.stead.ing"))
		Expect(mint.With).To(HaveKeyWithValue("permission-actions", "write"))
		Expect(start.Env).To(HaveKeyWithValue("GH_TOKEN", "${{ steps."+mint.ID+".outputs.token }}"))
		Expect(start.Run).To(Equal("gh workflow run publish.yml -R steading-ai/pkgs.stead.ing -f action=sync"))

		for name, other := range w.Jobs {
			Expect(other.Needs).NotTo(ContainElement("dispatch"), name)
		}
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
