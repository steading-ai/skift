package main_test

import (
	"os/exec"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// semver matches a semantic version with optional pre-release and build
// metadata, as the Makefile injects it.
const semver = `\b\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?\b`

// run executes the built binary with the given arguments and waits for exit.
func run(binary string, args ...string) *gexec.Session {
	cmd := exec.Command(binary, args...)
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait()
}

var _ = Describe("dg", Label("unit"), func() {
	var binary string

	BeforeEach(func() {
		// The same link step the Makefile performs, with a known version.
		var err error
		binary, err = gexec.Build(
			"github.com/ivy/dagger/cmd/dg",
			"-ldflags", "-X github.com/ivy/dagger/internal/version.version=1.2.3-spec+abc123",
		)
		Expect(err).NotTo(HaveOccurred())
	})

	It("The binary reports its version", func() {
		session := run(binary, "--version")
		Expect(session).To(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(MatchRegexp(semver))
		Expect(string(session.Out.Contents())).To(ContainSubstring("1.2.3-spec+abc123"))
	})

	It("The binary is a single entry point", func() {
		session := run(binary, "--help")
		Expect(session).To(gexec.Exit(0))
		help := string(session.Out.Contents())
		Expect(help).To(ContainSubstring("Available Commands:"))
		Expect(help).To(ContainSubstring("mcp"))
		Expect(help).To(ContainSubstring("serve"))
		Expect(help).To(ContainSubstring("version"))

		By("version is the only subcommand that does anything")
		Expect(run(binary, "version")).To(gexec.Exit(0))
		for _, face := range []string{"mcp", "serve"} {
			session := run(binary, face)
			Expect(session).NotTo(gexec.Exit(0), face)
			Expect(string(session.Err.Contents())).To(ContainSubstring("not implemented"), face)
		}
	})
})
