package skift_test

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The README is the front door: what skift is, how to install it, and
// where everything else is. How-to and reference live under docs/.
var _ = Describe("The README", Label("unit"), func() {
	var readme string

	BeforeEach(func() {
		raw, err := os.ReadFile("README.md")
		Expect(err).NotTo(HaveOccurred())
		readme = string(raw)
	})

	It("The README no longer describes the retired backlog", func() {
		By("when it is searched for the paper test of readiness and for a package no repository serves")
		// Joined here so that this file names nothing retired itself;
		// archive_test.go searches it too.
		paperTest := "scripts/" + "ready.py"
		unservedPackage := "dnf install skift"

		By("then neither is found")
		Expect(readme).NotTo(ContainSubstring(paperTest))
		Expect(readme).NotTo(ContainSubstring(unservedPackage))
	})

	It("is short enough to read in a minute", func() {
		Expect(strings.Count(readme, "\n")).To(BeNumerically("<", 80))
	})
})
