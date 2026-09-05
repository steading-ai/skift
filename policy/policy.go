// Package policy carries the default authorization bundle: the Rego under
// this directory, embedded so the binary ships with its policy. The Go
// side that compiles and evaluates it is internal/policy.
package policy

import "embed"

// FS holds every module of the default bundle, and its opa test suite.
//
//go:embed *.rego
var FS embed.FS
