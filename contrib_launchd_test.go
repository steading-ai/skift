package skift_test

import (
	"encoding/xml"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// launchAgent is the macOS launch agent a person copies into
// ~/Library/LaunchAgents to run their own skift serve.
const launchAgent = "contrib/launchd/ing.stead.skift.plist"

// plistDict is a property list's top-level dict, read as the alternating
// keys and values launchd reads: each <key> names the element after it.
type plistDict struct {
	Dict struct {
		Items []plistItem `xml:",any"`
	} `xml:"dict"`
}

type plistItem struct {
	XMLName xml.Name
	Text    string      `xml:",chardata"`
	Strings []string    `xml:"string"`
	Inner   []plistItem `xml:",any"`
}

// values maps each key of the dict to the element that follows it.
func (d plistDict) values() map[string]plistItem {
	out := map[string]plistItem{}
	items := d.Dict.Items
	for i := 0; i+1 < len(items); i += 2 {
		if items[i].XMLName.Local == "key" {
			out[items[i].Text] = items[i+1]
		}
	}
	return out
}

var _ = Describe("the launchd agent", Label("unit"), func() {
	var agent map[string]plistItem

	BeforeEach(func() {
		raw, err := os.ReadFile(launchAgent)
		Expect(err).NotTo(HaveOccurred())
		var d plistDict
		Expect(xml.Unmarshal(raw, &d)).To(Succeed())
		agent = d.values()
	})

	It("is labelled with the domain it is named for", func() {
		Expect(agent["Label"].Text).To(Equal("ing.stead.skift"))
	})

	It("runs skift serve on the default socket, the one the CLI and skift mcp dial", func() {
		args := agent["ProgramArguments"].Strings
		Expect(args).To(HaveLen(2))
		Expect(args[1]).To(Equal("serve"))
		Expect(args).NotTo(ContainElement(HavePrefix("--socket")), "a socket named here would differ from the one clients default to")
	})

	It("gives the service its database through the environment", func() {
		env := agent["EnvironmentVariables"]
		Expect(env.Inner).To(ContainElement(HaveField("Text", "DATABASE_URL")))
	})

	It("starts at login and restarts after a crash but not a clean stop", func() {
		Expect(agent["RunAtLoad"].XMLName.Local).To(Equal("true"))
		keepAlive := agent["KeepAlive"].Inner
		Expect(keepAlive).To(HaveLen(2))
		Expect(keepAlive[0].Text).To(Equal("SuccessfulExit"))
		Expect(keepAlive[1].XMLName.Local).To(Equal("false"))
	})

	It("waits out the service's drain before killing it", func() {
		Expect(agent["ExitTimeOut"].Text).To(Equal("45"))
	})
})
