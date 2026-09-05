package main_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/server"
)

// unreachableDB is a URL nothing listens on, so the service starts and its
// health check has something to report.
const unreachableDB = "postgres://dagger:dagger@127.0.0.1:1/dagger?sslmode=disable&connect_timeout=1"

// socketClient dials every request through the unix socket at path.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

var _ = Describe("dg serve", Label("unit"), func() {
	var (
		binary  string
		socket  string
		session *gexec.Session
	)

	BeforeEach(func() {
		var err error
		binary, err = gexec.Build("github.com/ivy/dagger/cmd/dg")
		Expect(err).NotTo(HaveOccurred())
		socket = filepath.Join(GinkgoT().TempDir(), "dg.sock")

		cmd := exec.Command(binary, "serve",
			"--socket", socket,
			"--socket-mode", "0600",
			"--database-url", unreachableDB,
			"--shutdown-timeout", "10s")
		session, err = gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			session.Kill().Wait(5 * time.Second)
		})
		Eventually(func() error {
			_, err := os.Lstat(socket)
			return err
		}).Should(Succeed(), "the socket never appeared")
	})

	It("serves the Connect handlers over the socket", func(ctx SpecContext) {
		By("when a client calls GetProject over the socket")
		client := daggerv1connect.NewGraphServiceClient(socketClient(socket), "http://dagger")
		_, err := client.GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))

		By("then the handler answers: unimplemented until the handler node lands, but over a working transport")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))

		By("and the socket carries the requested permissions")
		mode, _, err := server.SocketOwner(socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode).To(Equal(os.FileMode(0o600)))
	})

	It("reports database reachability on the health endpoint", func(ctx SpecContext) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dagger"+server.HealthPath, nil)
		Expect(err).NotTo(HaveOccurred())
		res, err := socketClient(socket).Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer res.Body.Close() //nolint:errcheck // response body of a finished read

		Expect(res).To(HaveHTTPStatus(http.StatusServiceUnavailable))
		var body server.Health
		Expect(json.NewDecoder(res.Body).Decode(&body)).To(Succeed())
		Expect(body.Status).To(Equal("degraded"))
		Expect(body.Database).NotTo(Equal("ok"))
	})

	It("exits cleanly on SIGTERM and removes its socket", func() {
		By("when the server receives SIGTERM")
		session.Signal(syscall.SIGTERM)

		By("then it drains and exits zero")
		Eventually(session, 15*time.Second).Should(gexec.Exit(0))
		Expect(string(session.Err.Contents())).To(ContainSubstring("shutdown signal received"))
		_, err := os.Lstat(socket)
		Expect(err).To(MatchError(os.ErrNotExist))
	})
})
