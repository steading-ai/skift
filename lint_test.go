package dagger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// writeFile creates a Go source file inside a copied checkout, making any
// directories it needs.
func writeFile(root, rel, content string) {
	path := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
	Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
}

// appendToFile adds content to the end of an existing file in a copied
// checkout.
func appendToFile(root, rel, content string) {
	path := filepath.Join(root, rel)
	existing, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(path, append(existing, content...), 0o644)).To(Succeed())
}

// makeLint runs `make lint` in the given checkout and waits for it to exit.
// Every package compiles from the shared build cache, so a couple of
// minutes covers a cold golangci-lint run with margin.
func makeLint(dir string) *gexec.Session {
	cmd := exec.Command("make", "lint")
	cmd.Dir = dir
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(2 * time.Minute)
}

var _ = Describe("Lint and package boundaries", Label("unit", "slow"), func() {
	BeforeEach(func() {
		if os.Getenv(nestedRun) != "" {
			Skip("already inside make test-unit")
		}
	})

	It("make lint passes on the current tree", func() {
		Expect(makeLint(checkout())).To(gexec.Exit(0))
	})

	It("Handlers may not reach the store directly", func() {
		By(`given a file under "internal/server" importing "internal/store"`)
		dir := checkout()
		// The store package has no Go files yet; the import must resolve
		// for depguard to see it rather than the type checker.
		writeFile(dir, "internal/store/store.go", "package store\n\n// Open is a placeholder.\nfunc Open() {}\n")
		writeFile(dir, "internal/server/handler.go",
			"package server\n\nimport \"github.com/ivy/dagger/internal/store\"\n\n// Handle reaches the store directly.\nfunc Handle() { store.Open() }\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails naming the depguard rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/server/handler.go"),
			ContainSubstring("(depguard)"),
		))
	})

	It("Generated code is current", func() {
		By("given a modified proto file")
		dir := checkout()
		appendToFile(dir, "proto/dagger/v1/dagger.proto", `
// Stale exists only to make gen/ lag the proto in this spec.
message Stale {
  // value carries nothing.
  string value = 1;
}
`)

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails saying generated code is stale")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(ContainSubstring("generated code is stale"))
	})

	It("Specs stay in Ginkgo", func() {
		By(`given a test file that is not a suite bootstrap importing "testing"`)
		dir := checkout()
		writeFile(dir, "internal/version/helper_test.go",
			"package version_test\n\nimport \"testing\"\n\nvar _ = func(t *testing.T) { t.Helper() }\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails naming the forbidigo rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/version/helper_test.go"),
			ContainSubstring("(forbidigo)"),
		))
	})
})
