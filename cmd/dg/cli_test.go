package main_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"google.golang.org/protobuf/encoding/protojson"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/dagger/internal/server"
)

// workStub answers Ready with one node, so the binary has something to
// print.
type workStub struct {
	daggerv1connect.UnimplementedWorkServiceHandler
}

func (workStub) Ready(_ context.Context, _ *connect.Request[daggerv1.ReadyRequest]) (*connect.Response[daggerv1.ReadyResponse], error) {
	return connect.NewResponse(&daggerv1.ReadyResponse{Nodes: []*daggerv1.Node{
		{Number: 7, Title: "Piped work", Status: daggerv1.Status_STATUS_OPEN, Priority: daggerv1.Priority_PRIORITY_P1, Ready: true},
	}}), nil
}

// serveStub mounts the stub on a socket for the length of the spec.
func serveStub() string {
	GinkgoHelper()
	socket := filepath.Join(GinkgoT().TempDir(), "dg.sock")
	srv := server.New(server.Config{SocketPath: socket, ShutdownTimeout: 2 * time.Second, Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))}, server.Services{Work: workStub{}}, nil)
	Expect(srv.Listen()).To(Succeed())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(srv.Serve(ctx)).To(Succeed())
	}()
	DeferCleanup(func() {
		cancel()
		Eventually(stopped, 5*time.Second).Should(BeClosed())
	})
	return socket
}

var _ = Describe("dg as a client", Label("unit"), func() {
	var binary, socket string

	BeforeEach(func() {
		var err error
		binary, err = gexec.Build("github.com/ivy/dagger/cmd/dg")
		Expect(err).NotTo(HaveOccurred())
		socket = serveStub()
	})

	It("Output is parseable when piped", func() {
		By(`when I run "dg ready" with stdout redirected to a file`)
		path := filepath.Join(GinkgoT().TempDir(), "ready.out")
		file, err := os.Create(path)
		Expect(err).NotTo(HaveOccurred())
		// The process's stdout is the file itself, not a pipe a harness
		// copies from, so what the binary sees is exactly a redirect.
		cmd := exec.Command(binary, "--socket", socket, "--project", "platform", "ready")
		cmd.Stdout = file
		cmd.Stderr = GinkgoWriter
		cmd.Env = append(os.Environ(), "DG_CONFIG=", "DG_ADDRESS=", "DG_SOCKET=")
		Expect(cmd.Run()).To(Succeed(), "dg ready exited non-zero")
		Expect(file.Close()).To(Succeed())

		By("then the file contains valid JSON")
		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Valid(raw)).To(BeTrue(), "not JSON: %s", raw)
		res := &daggerv1.ReadyResponse{}
		Expect(protojson.Unmarshal(raw, res)).To(Succeed())
		Expect(res.GetNodes()).To(HaveLen(1))
		Expect(res.GetNodes()[0].GetNumber()).To(BeEquivalentTo(7))
	})

	It("A failed command exits non-zero with a JSON error on stderr", func() {
		session := run(binary, "--socket", socket, "--project", "platform", "explain", "#7")
		Expect(session).To(gexec.Exit(1))
		Expect(session.Out.Contents()).To(BeEmpty())
		var e struct{ Code, Message string }
		Expect(json.Unmarshal(session.Err.Contents(), &e)).To(Succeed(), "stderr: %s", session.Err.Contents())
		Expect(e.Code).To(Equal("unimplemented"), "the stub mounts no GraphService")
		Expect(e.Message).NotTo(BeEmpty())
	})
})
