package server_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/server"
)

type isolatedTransport struct {
	listener net.Listener
	handler  http.Handler
	failure  error
	closed   atomic.Int32
}

func (t *isolatedTransport) Name() string { return "isolated-test" }
func (t *isolatedTransport) Listen() (net.Listener, error) {
	if t.failure != nil {
		return nil, t.failure
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	t.listener = l
	return l, err
}
func (t *isolatedTransport) ConnContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
func (t *isolatedTransport) Close() error                                                { t.closed.Add(1); return nil }
func (t *isolatedTransport) HTTPHandler() http.Handler                                   { return t.handler }

var _ = Describe("Isolated MCP listener lifecycle", Label("unit"), func() {
	It("isolates MCP from Unix while existing Connect still works", func(ctx SpecContext) {
		transport := &isolatedTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mcp" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})}
		r := start(server.Config{Transports: []server.Transport{transport}}, server.Services{Graph: graphStub{}}, pinger{})
		res := r.httpGet("/mcp")
		Expect(res.StatusCode).To(Equal(http.StatusNotFound))
		Expect(res.Body.Close()).To(Succeed())
		res, err := http.Get("http://" + transport.listener.Addr().String() + "/mcp")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.StatusCode).To(Equal(http.StatusNoContent))
		Expect(res.Body.Close()).To(Succeed())
		_, err = r.graph().GetProject(ctx, &skiftv1.GetProjectRequest{Project: "test"})
		Expect(err).NotTo(HaveOccurred())
	})
	It("unwinds earlier listeners and closes the shared transport exactly once after TLS startup failure", func() {
		parent := &isolatedTransport{handler: http.NotFoundHandler()}
		tls := &isolatedTransport{failure: errors.New("TLS certificate configuration failed")}
		path := filepath.Join(GinkgoT().TempDir(), "skift.sock")
		srv := server.New(server.Config{SocketPath: path, Transports: []server.Transport{parent, tls}, Logger: specLogger()}, server.Services{}, pinger{})
		Expect(srv.Listen()).To(MatchError(ContainSubstring("TLS certificate configuration failed")))
		Expect(parent.closed.Load()).To(Equal(int32(1)))
		_, err := net.Dial("tcp", parent.listener.Addr().String())
		Expect(err).To(HaveOccurred())
		_, err = os.Stat(path)
		Expect(err).To(MatchError(os.ErrNotExist))
	})
	It("passes the common drain signal to isolated requests", func() {
		entered := make(chan struct{}, 1)
		drained := make(chan struct{}, 1)
		transport := &isolatedTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-server.Draining(r.Context())
			drained <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		})}
		r := start(server.Config{Transports: []server.Transport{transport}}, server.Services{}, pinger{})
		done := make(chan error, 1)
		go func() {
			res, err := http.Get("http://" + transport.listener.Addr().String() + "/mcp")
			if err == nil {
				err = res.Body.Close()
			}
			done <- err
		}()
		Eventually(entered).Should(Receive())
		Expect(r.stop()).To(Succeed())
		Eventually(drained).Should(Receive())
		Eventually(done).Should(Receive(BeNil()))
	})
	It("cancels lingering isolated request contexts at the shared shutdown deadline", func() {
		entered := make(chan struct{}, 1)
		canceled := make(chan struct{}, 1)
		transport := &isolatedTransport{handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
		})}
		r := start(server.Config{ShutdownTimeout: 20 * time.Millisecond, Transports: []server.Transport{transport}}, server.Services{}, pinger{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			res, err := http.Get("http://" + transport.listener.Addr().String() + "/mcp")
			if err == nil {
				_ = res.Body.Close()
			}
		}()
		Eventually(entered).Should(Receive())
		r.cancel()
		Eventually(r.stopped).Should(BeClosed())
		Expect(r.err).To(MatchError(ContainSubstring("deadline exceeded")))
		Eventually(canceled).Should(Receive())
		Eventually(done).Should(BeClosed())
		// start's cleanup sees the expected timeout too; consume it after asserting.
		r.err = nil
	})
})
