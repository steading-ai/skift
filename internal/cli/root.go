// Package cli assembles the cobra command tree for the dg binary.
//
// One binary has three faces: dg the CLI, dg mcp the stdio MCP server, and
// dg serve the service. The CLI commands are thin: each builds one request,
// sends it through the shared client, and prints the response. Every
// command emits JSON when asked, or whenever stdout is not a terminal, and
// every failure is a JSON object on stderr with a non-zero exit code.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ivy/dagger/internal/client"
	"github.com/ivy/dagger/internal/version"
)

// ErrNotImplemented is returned by a face of the binary that is wired into the
// command tree but has no behaviour yet.
var ErrNotImplemented = errors.New("not implemented")

// Run executes the binary with args and returns its exit status. Failures
// are written to stderr as JSON; nothing else reaches stderr from the CLI
// face. main is a call to this.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := NewRootCommand(stdout, stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		writeError(stderr, err)
	}
	return ExitCode(err)
}

// NewRootCommand builds the dg command tree. Output is written to stdout and
// diagnostics to stderr; callers, including specs, may redirect either.
func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:     "dg",
		Short:   "Agent-first project management on a directed acyclic graph",
		Version: version.String(),
		Long: `dg is the single entry point to dagger.

Run without a subcommand it is the CLI, a Connect client with JSON output.
"dg mcp" serves the same client over stdio as an MCP server.
"dg serve" runs the service: Connect handlers over PostgreSQL.
"dg migrate" brings the database to the current schema.
"dg admin bootstrap" creates the first organization and principal.
"dg reap" expires stale leases; dagger-reap.timer runs it.

Node references are "#7" in the current project, "slug#7" in another
project of the organization, or "org/slug#7" fully qualified. The current
project comes from --project, $DG_PROJECT, or the config file.

Output is JSON whenever stdout is not a terminal, and on request with
--output json. Errors are a JSON object on stderr: {"code": ..., "message": ...}.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	// A bad flag is a usage error like any other, reported in the same
	// shape.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: err}
	})

	pf := root.PersistentFlags()
	pf.StringVar(&a.configPath, "config", "", "config file (default: $"+client.EnvConfig+", then "+client.DefaultConfigPath()+")")
	pf.StringVar(&a.flags.Socket, "socket", "", "unix socket of the service (default: $"+client.EnvSocket+", then "+client.DefaultSocketPath()+")")
	pf.StringVar(&a.flags.Address, "address", "", "URL of the service over the tailnet or HTTPS, instead of the socket (default: $"+client.EnvAddress+")")
	pf.StringArrayVarP(&a.projects, "project", "p", nil, "project a bare #7 reference resolves in, or a filter for ready, claim, and list; repeatable (default: $"+client.EnvProject+", or every project the caller may read)")
	pf.StringVarP(&a.output, "output", "o", "", "output format: json or text (default: json unless stdout is a terminal)")

	root.AddCommand(
		newVersionCommand(),
		a.mcpCommand(),
		newServeCommand(),
		newMigrateCommand(),
		newAdminCommand(),
		newReapCommand(),
		a.projectCommand(),
		a.nodeCommand(),
		a.patchCommand(),
		a.createCommand(),
		a.updateCommand(),
		a.linkCommand(),
		a.dropCommand(),
		a.holdCommand(),
		a.unholdCommand(),
		a.focusCommand(),
		a.readyCommand(),
		a.explainCommand(),
		a.inputsCommand(),
		a.claimCommand(),
		a.heartbeatCommand(),
		a.releaseCommand(),
		a.completeCommand(),
		a.estimateCommand(),
		a.usageCommand(),
		a.commentCommand(),
		a.searchCommand(),
		a.eventsCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this build",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "dg version %s\n", version.String())
			return err
		},
	}
}
