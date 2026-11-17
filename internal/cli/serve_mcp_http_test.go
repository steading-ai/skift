package cli_test

import (
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/cli"
)

var _ = Describe("Native MCP HTTPS configuration", Label("unit"), func() {
	BeforeEach(func() {
		GinkgoT().Setenv("SKIFT_MCP_HTTP", "")
		GinkgoT().Setenv("SKIFT_MCP_HTTP_PORT", "")
		GinkgoT().Setenv("SKIFT_TAILNET_HOSTNAME", "")
	})
	run := func(args ...string) error {
		cmd := cli.NewRootCommand(io.Discard, io.Discard)
		cmd.SetArgs(append([]string{"serve", "--database-url", unreachableDB}, args...))
		return cmd.Execute()
	}
	It("is opt-in and defaults to TLS port 443", func() {
		cmd := cli.NewRootCommand(io.Discard, io.Discard)
		serve, _, err := cmd.Find([]string{"serve"})
		Expect(err).NotTo(HaveOccurred())
		Expect(serve.Flags().Lookup("mcp-http").DefValue).To(Equal("false"))
		Expect(serve.Flags().Lookup("mcp-http-port").DefValue).To(Equal("443"))
	})
	It("requires the existing tailnet configuration", func() {
		Expect(run("--mcp-http")).To(MatchError(ContainSubstring("needs --tailnet-hostname")))
	})
	It("rejects a collision before enrollment", func() {
		Expect(run("--mcp-http", "--tailnet-hostname", "skift", "--mcp-http-port", "80")).To(MatchError(ContainSubstring("collides")))
	})
	It("rejects zero and out-of-range ports", func() {
		Expect(run("--mcp-http", "--tailnet-hostname", "skift", "--mcp-http-port", "0")).To(MatchError(ContainSubstring("must be from 1 to 65535")))
		Expect(run("--mcp-http-port", "65536")).To(HaveOccurred())
	})
	It("reads the environment and lets explicit flags override it", func() {
		GinkgoT().Setenv("SKIFT_MCP_HTTP", "true")
		Expect(run()).To(MatchError(ContainSubstring("needs --tailnet-hostname")))
		GinkgoT().Setenv("SKIFT_MCP_HTTP_PORT", "80")
		Expect(run("--tailnet-hostname", "skift")).To(MatchError(ContainSubstring("collides")))
		Expect(run("--tailnet-hostname", "skift", "--mcp-http-port", "443", "--tailnet-tag", "invalid")).To(MatchError(ContainSubstring("tag:name")))
	})
	It("lets explicit disabled flags override invalid environment values", func() {
		GinkgoT().Setenv("SKIFT_MCP_HTTP", "invalid")
		GinkgoT().Setenv("SKIFT_MCP_HTTP_PORT", "invalid")
		Expect(run("--mcp-http=false", "--mcp-http-port", "443", "--socket-mode", "invalid")).To(MatchError(ContainSubstring("--socket-mode")))
	})
	It("rejects invalid environment configuration", func() {
		GinkgoT().Setenv("SKIFT_MCP_HTTP", "yes-please")
		Expect(run()).To(MatchError(ContainSubstring("SKIFT_MCP_HTTP")))
		GinkgoT().Setenv("SKIFT_MCP_HTTP", "")
		GinkgoT().Setenv("SKIFT_MCP_HTTP_PORT", "-1")
		Expect(run()).To(MatchError(ContainSubstring("SKIFT_MCP_HTTP_PORT")))
	})
	It("validates the shared node configuration before TLS enrollment", func() {
		Expect(run("--mcp-http", "--tailnet-hostname", "skift", "--tailnet-tag", "invalid")).To(MatchError(ContainSubstring("tag:name")))
	})
})
