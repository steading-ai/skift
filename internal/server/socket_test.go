package server_test

import (
	"net"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/server"
)

var _ = Describe("The socket", Label("unit"), func() {
	newServer := func(path string) *server.Server {
		return server.New(server.Config{SocketPath: path, Logger: specLogger()}, server.Services{Graph: graphStub{}}, pinger{})
	}

	It("needs a path, and refuses one that holds something other than a socket", func() {
		Expect(newServer("").Listen()).To(MatchError(ContainSubstring("socket path is required")))
		path := filepath.Join(GinkgoT().TempDir(), "skift.sock")
		Expect(os.WriteFile(path, []byte("not a socket"), 0o600)).To(Succeed())
		Expect(newServer(path).Listen()).To(MatchError(ContainSubstring("is not a socket")))
	})

	It("clears a socket nobody serves, and leaves one another server holds", func() {
		dir := GinkgoT().TempDir()

		By("given a socket file left behind by a server that is gone")
		stale := filepath.Join(dir, "stale.sock")
		l, err := net.Listen("unix", stale)
		Expect(err).NotTo(HaveOccurred())
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		Expect(l.Close()).To(Succeed())
		Expect(stale).To(BeAnExistingFile())

		By("then a server takes the path over")
		r := start(server.Config{SocketPath: stale}, server.Services{Graph: graphStub{}}, pinger{})
		Expect(r.stop()).To(Succeed())

		By("but not one another server is listening on")
		live := filepath.Join(dir, "live.sock")
		held, err := net.Listen("unix", live)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(held.Close)
		Expect(newServer(live).Listen()).To(MatchError(ContainSubstring("in use by another server")))
	})
})
