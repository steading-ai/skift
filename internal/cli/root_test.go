package cli_test

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/cli"
	"github.com/ivy/dagger/internal/version"
)

var _ = Describe("root command", Label("unit"), func() {
	var stdout, stderr *bytes.Buffer

	execute := func(args ...string) error {
		stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
		root := cli.NewRootCommand(stdout, stderr)
		// A nil slice makes cobra read os.Args, which under go test are the
		// test flags; always hand it an explicit list.
		root.SetArgs(append([]string{}, args...))
		return root.Execute()
	}

	It("prints help when run without a subcommand", func() {
		Expect(execute()).To(Succeed())
		Expect(stdout.String()).To(ContainSubstring("Available Commands:"))
	})

	It("reports the linked version through --version", func() {
		Expect(execute("--version")).To(Succeed())
		Expect(stdout.String()).To(ContainSubstring(version.String()))
	})

	It("wires the three faces of the binary", func() {
		root := cli.NewRootCommand(stdout, stderr)
		names := []string{}
		for _, c := range root.Commands() {
			names = append(names, c.Name())
		}
		Expect(names).To(ConsistOf("version", "mcp", "serve"))
	})

	DescribeTable("faces without behaviour fail loudly",
		func(face string) {
			Expect(execute(face)).To(MatchError(cli.ErrNotImplemented))
		},
		Entry("mcp", "mcp"),
		Entry("serve", "serve"),
	)

	It("rejects unknown subcommands", func() {
		Expect(execute("bogus")).To(HaveOccurred())
	})
})
