// Command dg is the single entry point to dagger: the CLI, the stdio MCP
// server, and the service.
package main

import (
	"fmt"
	"os"

	"github.com/ivy/dagger/internal/cli"
)

func main() {
	root := cli.NewRootCommand(os.Stdout, os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "dg:", err)
		os.Exit(1)
	}
}
