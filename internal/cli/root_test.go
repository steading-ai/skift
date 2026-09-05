package cli_test

import (
	"bytes"
	"path/filepath"

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

	It("wires the three faces of the binary and every CLI command", func() {
		root := cli.NewRootCommand(stdout, stderr)
		names := []string{}
		for _, c := range root.Commands() {
			names = append(names, c.Name())
		}
		Expect(names).To(ConsistOf("version", "mcp", "serve", "reap",
			"project", "node", "patch", "create", "link", "ready", "explain",
			"claim", "heartbeat", "release", "complete", "comment", "search", "events"))
	})

	Describe("serve", func() {
		It("requires a database URL", func() {
			GinkgoT().Setenv("DATABASE_URL", "")
			err := execute("serve", "--socket", filepath.Join(GinkgoT().TempDir(), "dg.sock"))
			Expect(err).To(MatchError(ContainSubstring("database URL is required")))
		})

		It("rejects a socket mode that is not octal permissions", func() {
			err := execute("serve",
				"--socket", filepath.Join(GinkgoT().TempDir(), "dg.sock"),
				"--database-url", "postgres://127.0.0.1:1/dagger",
				"--socket-mode", "rw-rw----")
			Expect(err).To(MatchError(ContainSubstring("--socket-mode")))
		})

		It("rejects an unknown log level", func() {
			err := execute("serve", "--database-url", "postgres://127.0.0.1:1/dagger", "--log-level", "loud")
			Expect(err).To(MatchError(ContainSubstring("--log-level")))
		})
	})

	It("rejects unknown subcommands", func() {
		Expect(execute("bogus")).To(HaveOccurred())
	})
})
