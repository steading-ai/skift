package skift_test

import (
	"os"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// pinnedIn returns the one thing the pattern's group captures in a file.
func pinnedIn(file, pattern string) string {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())
	found := regexp.MustCompile(pattern).FindAllSubmatch(raw, -1)
	Expect(found).To(HaveLen(1), "%s holds one match of %s", file, pattern)
	return string(found[0][1])
}

// Some versions are written down twice because two things read them.
// renovate.json5 groups each pair so that one pull request moves both;
// these specs fail a change that moves only one.
var _ = Describe("A version pinned in two files", Label("unit"), func() {
	It("is one Go toolchain: the one mise installs is the one go.mod names", func() {
		Expect(pinnedIn("mise.toml", `(?m)^go = "([^"]+)"$`)).To(Equal(pinnedIn("go.mod", `(?m)^go (\S+)$`)))
	})

	It("is one PostgreSQL image: the database specs start the one compose.yaml runs", func() {
		image := pinnedIn("compose.yaml", `(?m)^\s+image: (\S*/postgres:\S+)$`)
		Expect(image).To(MatchRegexp(`@sha256:[0-9a-f]{64}$`), "the image is pinned by digest")
		Expect(pinnedIn("internal/testsupport/postgres.go", `(?m)^const postgresImage = "([^"]+)"$`)).To(Equal(image))
	})
})
