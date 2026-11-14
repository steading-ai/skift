package skift_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// .github/workflows/release-notes.yml asks a model to write a release's
// notes and may replace them. What can start it, what it may do, and that
// the repository hands it no model credential are the properties
// docs/adrs/011-releases-are-immutable-and-attested.md decides in decision
// 8, so they are held here rather than left to review.
var _ = Describe("The release-notes workflow", Label("unit"), func() {
	var w workflow

	BeforeEach(func() {
		w = loadWorkflow("release-notes.yml")
	})

	It("can be called or dispatched, and nothing else", func() {
		Expect(w.triggers()).To(ConsistOf("workflow_call", "workflow_dispatch"))
		for name, trigger := range w.On {
			Expect(trigger.Inputs).To(HaveKey("tag"), name)
			Expect(trigger.Inputs).To(HaveKeyWithValue("dry_run", SatisfyAll(
				HaveKeyWithValue("type", "boolean"),
				HaveKeyWithValue("default", false),
			)), "%s: a run publishes unless it is asked not to", name)
		}
	})

	It("holds no model credential", func() {
		Expect(w.secrets()).To(SatisfyAll(Not(BeEmpty()), HaveEach("GITHUB_TOKEN")),
			"the only secret is the token GitHub gives every job")

		Expect(w.Permissions).To(BeEmpty(), "top-level permissions are empty; the job states its own")
		Expect(w.Jobs).To(HaveLen(1))
		for name, job := range w.Jobs {
			Expect(job.Permissions).To(HaveKeyWithValue("id-token", "write"),
				"%s: its identity is what it exchanges for a model token", name)
			Expect(job.Permissions).To(Equal(map[string]string{
				"id-token":      "write",
				"contents":      "write",
				"pull-requests": "read",
			}), name)
			Expect(job.Environment).To(Equal("release"),
				"%s: the environment holds the identifiers the exchange names", name)
		}

		for _, prefix := range []string{"fdrl_", "svac_", "wrkspc_"} {
			Expect(w.source).NotTo(ContainSubstring(prefix),
				"the identifiers are the environment's variables, not the workflow's text")
		}
	})

	It("never lets the tag reach a shell by interpolation", func() {
		Expect(w.scripts()).NotTo(BeEmpty())
		for _, script := range w.scripts() {
			Expect(script).NotTo(ContainSubstring("${{"))
		}
	})

	It("masks each token in the step that obtains it", func() {
		var exchanges []string
		for _, script := range w.scripts() {
			if strings.Contains(script, "/v1/oauth/token") {
				exchanges = append(exchanges, script)
			}
		}
		Expect(exchanges).To(HaveLen(1), "an identity token is good for one exchange")
		Expect(exchanges[0]).To(ContainSubstring(`echo "::add-mask::$jwt"`))
		Expect(exchanges[0]).To(ContainSubstring(`echo "::add-mask::$access_token"`))
	})

	It("checks out every tag, and leaves no credential behind in the checkout", func() {
		checkouts := 0
		for _, job := range w.Jobs {
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					checkouts++
					Expect(step.With).To(HaveKeyWithValue("fetch-depth", 0))
					Expect(step.With).To(HaveKeyWithValue("persist-credentials", false))
				}
			}
		}
		Expect(checkouts).To(Equal(1))
	})

	It("publishes only when it is not a dry run", func() {
		var publishing []string
		for _, script := range w.scripts() {
			if strings.Contains(script, "--github-release") {
				publishing = append(publishing, script)
			}
		}
		Expect(publishing).To(HaveLen(1))

		// The flag appears once, on the line a dry run's guard governs.
		Expect(strings.Count(publishing[0], "--github-release")).To(Equal(1))
		at := map[string]int{}
		for i, command := range commands(publishing[0]) {
			at[strings.TrimSpace(command)] = i + 1
		}
		guard, publish := at[`if test "$DRY_RUN" != true; then`], at["flags+=(--github-release)"]
		Expect(guard).To(BeNumerically(">", 0))
		Expect(publish).To(Equal(guard + 1))
	})
})
