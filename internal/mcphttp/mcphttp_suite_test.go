package mcphttp_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMCPHTTP(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Native MCP HTTP Suite") }
