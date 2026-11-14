package mcptools_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMCPTools(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "shared MCP tools") }
