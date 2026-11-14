// Command skift is one binary with three faces: the CLI, the stdio MCP
// server, and the service.
package main

import (
	"os"

	// CA roots for TLS, consulted only when the host has no trust store.
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/steading-ai/skift/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
