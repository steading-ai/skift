package cli_test

import (
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"

	"github.com/steading-ai/skift/internal/cli"
)

// serveFlag builds the root command under the environment the spec set and
// returns one of serve's flags, so a default read from the environment is
// observed where cobra reads it.
func serveFlag(name string) *pflag.Flag {
	GinkgoHelper()
	root := cli.NewRootCommand(io.Discard, io.Discard)
	serve, _, err := root.Find([]string{"serve"})
	Expect(err).NotTo(HaveOccurred())
	flag := serve.Flags().Lookup(name)
	Expect(flag).NotTo(BeNil(), "serve has no --%s", name)
	return flag
}

var _ = Describe("skift serve tailnet configuration", Label("unit"), func() {
	BeforeEach(func() {
		for _, k := range []string{"SKIFT_TAILNET_HOSTNAME", "SKIFT_TAILNET_TAGS", "SKIFT_TAILNET_STATE_DIR", "STATE_DIRECTORY"} {
			GinkgoT().Setenv(k, "")
		}
	})

	It("Scenario: A unit joins the tailnet from its environment file alone", func() {
		By("given SKIFT_TAILNET_HOSTNAME and SKIFT_TAILNET_TAGS in the environment")
		GinkgoT().Setenv("SKIFT_TAILNET_HOSTNAME", "skift")
		GinkgoT().Setenv("SKIFT_TAILNET_TAGS", "tag:service, tag:platform,")

		By("then the tailnet flags default to them, the tags split on commas")
		Expect(serveFlag("tailnet-hostname").Value.String()).To(Equal("skift"))
		Expect(serveFlag("tailnet-tag").Value.String()).To(Equal("[tag:service,tag:platform]"))
	})

	It("keeps the node's state under the unit's StateDirectory by default", func() {
		GinkgoT().Setenv("STATE_DIRECTORY", "/var/lib/skift:/var/lib/other")
		Expect(serveFlag("tailnet-state-dir").Value.String()).To(Equal("/var/lib/skift/tailnet"))

		By("unless SKIFT_TAILNET_STATE_DIR names somewhere else")
		GinkgoT().Setenv("SKIFT_TAILNET_STATE_DIR", "/srv/tailnet")
		Expect(serveFlag("tailnet-state-dir").Value.String()).To(Equal("/srv/tailnet"))
	})

	It("leaves the tailnet off and the state dir to tsnet with nothing set", func() {
		Expect(serveFlag("tailnet-hostname").Value.String()).To(BeEmpty())
		Expect(serveFlag("tailnet-tag").Value.String()).To(Equal("[]"))
		Expect(serveFlag("tailnet-state-dir").Value.String()).To(BeEmpty())
	})
})
