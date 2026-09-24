package sdnotify_test

import (
	"net"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/sdnotify"
)

// listen opens a datagram socket the way systemd does for a notify unit,
// and returns a channel of every message it receives.
func listen(path string) <-chan string {
	GinkgoHelper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	got := make(chan string, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	return got
}

var _ = Describe("sdnotify", Label("unit"), func() {
	It("sends the states as one datagram to the notification socket", func() {
		path := filepath.Join(GinkgoT().TempDir(), "notify.sock")
		got := listen(path)

		sent, err := sdnotify.SendTo(path, sdnotify.Ready, sdnotify.Status("database reachable"))
		Expect(err).NotTo(HaveOccurred())
		Expect(sent).To(BeTrue())
		Eventually(got).Should(Receive(Equal("READY=1\nSTATUS=database reachable\n")))
	})

	It("does nothing outside a notify unit", func() {
		sent, err := sdnotify.SendTo("", sdnotify.Ready)
		Expect(err).NotTo(HaveOccurred())
		Expect(sent).To(BeFalse())
	})

	It("reports a socket nobody listens on", func() {
		sent, err := sdnotify.SendTo(filepath.Join(GinkgoT().TempDir(), "absent.sock"), sdnotify.Ready)
		Expect(err).To(HaveOccurred())
		Expect(sent).To(BeFalse())
	})

	It("folds a multi-line status onto one line", func() {
		Expect(sdnotify.Status("one\ntwo")).To(Equal("STATUS=one two"))
	})
})
