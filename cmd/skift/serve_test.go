package main_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/testsupport"
)

// unreachableDB is a URL nothing listens on, so the service starts and its
// health check has something to report.
const unreachableDB = "postgres://skift:skift@127.0.0.1:1/skift?sslmode=disable&connect_timeout=1"

// socketClient dials every request through the unix socket at path.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

var _ = Describe("skift serve", Label("unit"), func() {
	var (
		socket  string
		session *gexec.Session
	)

	BeforeEach(func() {
		var err error
		socket = filepath.Join(GinkgoT().TempDir(), "skift.sock")

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
		testsupport.RequirePeerCredentials()
		By("when a client calls GetProject over the socket")
		client := skiftv1connect.NewGraphServiceClient(socketClient(socket), "http://skift")
		_, err := client.GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "skeleton"}))

		By("then the service answers over the socket: the database is unreachable, so the caller cannot be resolved")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))

		By("and the socket carries the requested permissions")
		mode, _, err := server.SocketOwner(socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode).To(Equal(os.FileMode(0o600)))
	})

	It("reports database reachability on the health endpoint", func(ctx SpecContext) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://skift"+server.HealthPath, nil)
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

// shutdownRuns is how many SIGTERM cycles the ordering scenario runs. The
// missing log line was a race, so a single clean exit proves nothing.
const shutdownRuns = 5

var _ = Describe("skift serve shutdown", Label("unit"), func() {
	It("The shutdown log line always precedes exit", func() {
		dir := GinkgoT().TempDir()

		for i := range shutdownRuns {
			By("given skift serve is running with an unreachable database")
			socket := filepath.Join(dir, fmt.Sprintf("skift%d.sock", i))
			cmd := exec.Command(binary, "serve",
				"--socket", socket,
				"--database-url", unreachableDB,
				"--shutdown-timeout", "10s")
			session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				session.Kill().Wait(5 * time.Second)
			})
			Eventually(func() error {
				_, err := os.Lstat(socket)
				return err
			}).Should(Succeed(), "run %d: the socket never appeared", i)

			By("when it receives SIGTERM")
			session.Signal(syscall.SIGTERM)

			By("then it exits zero")
			Eventually(session, 15*time.Second).Should(gexec.Exit(0), "run %d", i)

			By("and its stderr contains the shutdown line, every time")
			Expect(string(session.Err.Contents())).
				To(ContainSubstring("shutdown signal received"), "run %d", i)

			By("and its socket is removed")
			_, err = os.Lstat(socket)
			Expect(err).To(MatchError(os.ErrNotExist), "run %d", i)
		}
	})
})

// health reads the health endpoint over the socket.
func health(ctx context.Context, socket string) (server.Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://skift"+server.HealthPath, nil)
	if err != nil {
		return server.Health{}, err
	}
	res, err := socketClient(socket).Do(req)
	if err != nil {
		return server.Health{}, err
	}
	defer res.Body.Close() //nolint:errcheck // response body of a finished read
	var body server.Health
	return body, json.NewDecoder(res.Body).Decode(&body)
}

var _ = Describe("skift serve and the schema", Label(testsupport.DBLabel), func() {
	var socket string

	BeforeEach(func() {
		socket = filepath.Join(GinkgoT().TempDir(), "skift.sock")
	})

	serve := func(url string, flags ...string) *gexec.Session {
		GinkgoHelper()
		args := append([]string{"serve", "--socket", socket, "--database-url", url, "--shutdown-timeout", "10s"}, flags...)
		session, err := gexec.Start(exec.Command(binary, args...), GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			session.Kill().Wait(5 * time.Second)
		})
		return session
	}

	It("migrates an empty database at start and serves", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)

		By("when skift serve starts against an empty database")
		session := serve(url)

		By("then it reports itself ready with the schema current")
		Eventually(session.Err, 30*time.Second).Should(gbytes.Say("database reachable; ready"))
		Expect(string(session.Err.Contents())).To(ContainSubstring("migration applied"))
		Expect(schemaVersion(ctx, url)).NotTo(BeZero())

		By("and it serves: the health endpoint answers ok over the socket")
		h, err := health(ctx, socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Status).To(Equal("ok"))

		By("and it exits zero on SIGTERM")
		session.Signal(syscall.SIGTERM)
		Eventually(session, 15*time.Second).Should(gexec.Exit(0))
	})

	It("with --no-migrate refuses an empty database rather than serving over it", func(ctx SpecContext) {
		url := pg.EmptyDatabase(ctx)

		By("when skift serve --no-migrate starts against an empty database")
		session := serve(url, "--no-migrate")

		By("then it exits non-zero naming skift migrate, applies nothing, and leaves no socket")
		Eventually(session, 30*time.Second).Should(gexec.Exit(cli.ExitFailure))
		Expect(string(session.Err.Contents())).To(ContainSubstring("run skift migrate"))
		Expect(schemaVersion(ctx, url)).To(BeZero())
		_, err := os.Lstat(socket)
		Expect(err).To(MatchError(os.ErrNotExist))
	})

	It("with --no-migrate serves a database that is already current", func(ctx SpecContext) {
		url := pg.Database(ctx).Config().ConnString()
		session := serve(url, "--no-migrate")
		Eventually(session.Err, 30*time.Second).Should(gbytes.Say("database reachable; ready"))
		Expect(string(session.Err.Contents())).NotTo(ContainSubstring("migration applied"))
		h, err := health(ctx, socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Status).To(Equal("ok"))
	})
})
