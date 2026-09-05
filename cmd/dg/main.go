// Command dg is the single entry point to dagger: the CLI, the stdio MCP
// server, and the service.
package main

import (
	"os"

	"github.com/ivy/dagger/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
