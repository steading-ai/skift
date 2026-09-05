// Package cli assembles the cobra command tree for the dg binary.
//
// One binary has three faces: dg the CLI, dg mcp the stdio MCP server, and
// dg serve the service. The tree is wired here so later work attaches
// commands to an existing parent rather than restructuring main.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ivy/dagger/internal/version"
)

// ErrNotImplemented is returned by a face of the binary that is wired into the
// command tree but has no behaviour yet.
var ErrNotImplemented = errors.New("not implemented")

// NewRootCommand builds the dg command tree. Output is written to stdout and
// diagnostics to stderr; callers, including specs, may redirect either.
func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:     "dg",
		Short:   "Agent-first project management on a directed acyclic graph",
		Version: version.String(),
		Long: `dg is the single entry point to dagger.

Run without a subcommand it is the CLI, a Connect client with JSON output.
"dg mcp" serves the same client over stdio as an MCP server.
"dg serve" runs the service: Connect handlers over PostgreSQL.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(
		newVersionCommand(),
		newMCPCommand(),
		newServeCommand(),
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

func newMCPCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the CLI as an MCP server over stdio",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("dg mcp: %w", ErrNotImplemented)
		},
	}
}

func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the dagger service",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("dg serve: %w", ErrNotImplemented)
		},
	}
}
