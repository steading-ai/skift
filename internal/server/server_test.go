package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
	"github.com/ivy/dagger/internal/server"
	"github.com/ivy/dagger/internal/testsupport"
)

var _ = Describe("Connect server over a unix socket", Label("unit"), func() {
	It("A client on the same host connects", func(ctx SpecContext) {
		By(`given "dg serve" is running on a unix socket`)
		r := start(server.Config{}, server.Services{Graph: graphStub{}}, pinger{})

		By("when a client calls GetProject over that socket")
		res, err := r.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{Project: "skeleton"}))

		By("then a response is returned")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Msg.GetProject().GetSlug()).To(Equal("skeleton"))
	})

	It("The generated Connect client reaches the server over the socket", func(ctx SpecContext) {
		By("given a server with nothing but the default, unimplemented services")
		r := start(server.Config{}, server.Services{}, pinger{})

		By("when the generated client calls each service")
		_, graphErr := r.graph().GetProject(ctx, connect.NewRequest(&daggerv1.GetProjectRequest{}))
		_, workErr := r.work().Ready(ctx, connect.NewRequest(&daggerv1.ReadyRequest{}))

		By("then every call is answered by the handler, as a Connect error rather than a 404")
		Expect(connect.CodeOf(graphErr)).To(Equal(connect.CodeUnimplemented))
		Expect(connect.CodeOf(workErr)).To(Equal(connect.CodeUnimplemented))
	})

	It("Socket file permissions restrict access to the intended group", func() {
		By("given a socket configured for one group with owner-and-group access")
		gid := os.Getgid()
		r := start(server.Config{
			SocketMode:  0o660,
			SocketGroup: strconv.Itoa(gid),
		}, server.Services{}, pinger{})

		By("when the socket is inspected")
		mode, owner, err := server.SocketOwner(r.socket)
		Expect(err).NotTo(HaveOccurred())

		By("then only the owner and that group may connect")
		Expect(mode).To(Equal(os.FileMode(0o660)))
		Expect(owner).To(Equal(gid))
	})

	It("resolves the socket group by name", func() {
		r := start(server.Config{SocketGroup: groupName(os.Getgid())}, server.Services{}, pinger{})
		mode, owner, err := server.SocketOwner(r.socket)
		Expect(err).NotTo(HaveOccurred())
		Expect(mode).To(Equal(server.DefaultSocketMode))
		Expect(owner).To(Equal(os.Getgid()))
	})

	It("refuses an unknown socket group rather than serving world-readable", func() {
		path := filepath.Join(GinkgoT().TempDir(), "dg.sock")
		srv := server.New(server.Config{
			SocketPath:  path,
			SocketGroup: "no-such-group-for-dagger",
			Logger:      specLogger(),
		}, server.Services{}, pinger{})
		Expect(srv.Listen()).To(MatchError(ContainSubstring("resolve socket group")))
		_, err := os.Lstat(path)
		Expect(err).To(MatchError(os.ErrNotExist))
	})

	It("replaces a stale socket and refuses a live one", func() {
		By("given a socket file nobody listens on")
		path := filepath.Join(GinkgoT().TempDir(), "dg.sock")
		stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		Expect(err).NotTo(HaveOccurred())
		// A crashed process leaves its socket file behind; Close would
		// unlink it, so keep it as a crash would.
		stale.SetUnlinkOnClose(false)
		Expect(stale.Close()).To(Succeed())
		_, err = os.Lstat(path)
		Expect(err).NotTo(HaveOccurred())

		By("when a server starts on the same path")
		r := start(server.Config{SocketPath: path}, server.Services{}, pinger{})

		By("then it takes the path over, and a second live server is refused")
		Expect(r.httpGet(server.HealthPath)).To(HaveHTTPStatus(http.StatusOK))
		second := server.New(server.Config{SocketPath: path, Logger: specLogger()}, server.Services{}, pinger{})
		Expect(second.Listen()).To(MatchError(ContainSubstring("in use")))
	})

	It("Shutdown does not lose in-flight work", func(ctx SpecContext) {
		By("given a client waiting in ClaimNext")
		work := newWorkStub()
		r := start(server.Config{}, server.Services{Work: work}, pinger{})
		type result struct {
			res *connect.Response[daggerv1.ClaimNextResponse]
			err error
		}
		results := make(chan result, 1)
		go func() {
			defer GinkgoRecover()
			res, err := r.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{
				RequestId: "wait-1",
				Project:   "skeleton",
				Wait:      durationpb.New(time.Minute),
			}))
			results <- result{res, err}
		}()
		Eventually(work.waiting).Should(Receive(), "the call never reached the handler")

		By("when the server receives SIGTERM")
		stopped := make(chan error, 1)
		go func() { stopped <- r.stop() }()

		By("then the call returns cleanly rather than erroring")
		var got result
		Eventually(results, 10*time.Second).Should(Receive(&got))
		Expect(got.err).NotTo(HaveOccurred())
		Expect(got.res.Msg.GetNode()).To(BeNil(), "nothing was claimed")

		By("and no lease is left in an inconsistent state")
		Expect(got.res.Msg.GetLease()).To(BeNil())
		Expect(work.claims.Load()).To(BeZero())
		Eventually(stopped).Should(Receive(BeNil()), "the drain finished without forcing connections closed")
	})

	It("Shutdown drains rather than dropping waiting ClaimNext calls", func(ctx SpecContext) {
		By("given several clients waiting in ClaimNext")
		work := newWorkStub()
		r := start(server.Config{}, server.Services{Work: work}, pinger{})
		const waiters = 5
		errs := make(chan error, waiters)
		for i := range waiters {
			go func() {
				defer GinkgoRecover()
				_, err := r.work().ClaimNext(ctx, connect.NewRequest(&daggerv1.ClaimNextRequest{
					RequestId: "wait-" + strconv.Itoa(i),
					Project:   "skeleton",
					Wait:      durationpb.New(time.Minute),
				}))
				errs <- err
			}()
		}
		for range waiters {
			Eventually(work.waiting).Should(Receive())
		}

		By("when the server shuts down")
		started := time.Now()
		Expect(r.stop()).To(Succeed())

		By("then every waiter returned cleanly, well before its own wait elapsed")
		for range waiters {
			Eventually(errs).Should(Receive(BeNil()))
		}
		Expect(time.Since(started)).To(BeNumerically("<", 30*time.Second))
	})

	It("removes the socket when it stops", func() {
		r := start(server.Config{}, server.Services{}, pinger{})
		Expect(r.stop()).To(Succeed())
		_, err := os.Lstat(r.socket)
		Expect(err).To(MatchError(os.ErrNotExist))
	})

	Describe("Health check endpoint reports database reachability", func() {
		It("reports a database that answers", func() {
			r := start(server.Config{}, server.Services{}, pinger{})
			res, body := r.health()
			Expect(res).To(HaveHTTPStatus(http.StatusOK))
			Expect(body).To(Equal(server.Health{Status: "ok", Database: "ok"}))
		})

		It("reports a database that does not answer", func() {
			r := start(server.Config{}, server.Services{}, pinger{err: errors.New("connection refused")})
			res, body := r.health()
			Expect(res).To(HaveHTTPStatus(http.StatusServiceUnavailable))
			Expect(body.Status).To(Equal("degraded"))
			Expect(body.Database).To(ContainSubstring("connection refused"))
		})

		It("reports database reachability against PostgreSQL", Label(testsupport.DBLabel), func(ctx SpecContext) {
			By("given a server whose database is a live PostgreSQL")
			pool := pg.Database(ctx)
			r := start(server.Config{}, server.Services{}, pool)

			By("when the health check is called")
			res, body := r.health()

			By("then it reports the database reachable")
			Expect(res).To(HaveHTTPStatus(http.StatusOK))
			Expect(body.Database).To(Equal("ok"))

			By("and once the pool is closed, unreachable")
			pool.Close()
			res, body = r.health()
			Expect(res).To(HaveHTTPStatus(http.StatusServiceUnavailable))
			Expect(body.Database).NotTo(Equal("ok"))
		})
	})
})

// health calls the endpoint over the socket and decodes the body.
func (r *running) health() (*http.Response, server.Health) {
	GinkgoHelper()
	res := r.httpGet(server.HealthPath)
	defer res.Body.Close() //nolint:errcheck // response body of a finished read
	var body server.Health
	Expect(json.NewDecoder(res.Body).Decode(&body)).To(Succeed())
	return res, body
}

func (r *running) httpGet(path string) *http.Response {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+path, nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := r.httpClient().Do(req)
	Expect(err).NotTo(HaveOccurred())
	return res
}
