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

var _ = Describe("skift", Label("unit"), func() {
	It("The binary reports its version", func() {
		session := run(binary, "--version")
		Expect(session).To(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(MatchRegexp(semver))
		Expect(string(session.Out.Contents())).To(ContainSubstring(specVersion))
	})

	It("The binary is a single entry point", func() {
		session := run(binary, "--help")
		Expect(session).To(gexec.Exit(0))
		help := string(session.Out.Contents())
		Expect(help).To(ContainSubstring("Available Commands:"))
		Expect(help).To(ContainSubstring("mcp"))
		Expect(help).To(ContainSubstring("serve"))
		Expect(help).To(ContainSubstring("version"))

		By("the mcp face starts and shuts down cleanly when stdin is closed")
		Expect(run(binary, "version")).To(gexec.Exit(0))
		session = run(binary, "mcp")
		Expect(session).To(gexec.Exit(0))
		Expect(session.Out.Contents()).To(BeEmpty(), "stdout carries only protocol messages")
		Expect(string(session.Err.Contents())).To(ContainSubstring("stdin closed"))
	})
})
