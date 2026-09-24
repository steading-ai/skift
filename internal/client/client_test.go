package client_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"
	"github.com/steading-ai/skift/internal/client"
)

// graphStub answers GetProject with the slug it was asked for, so a spec
// can tell that a call reached it.
type graphStub struct {
	skiftv1connect.UnimplementedGraphServiceHandler
}

func (graphStub) GetProject(_ context.Context, req *connect.Request[skiftv1.GetProjectRequest]) (*connect.Response[skiftv1.GetProjectResponse], error) {
	return connect.NewResponse(&skiftv1.GetProjectResponse{Project: &skiftv1.Project{Slug: req.Msg.GetProject()}}), nil
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(skiftv1connect.NewGraphServiceHandler(graphStub{}))
	return mux
}

// serveUnix serves the stub on a socket in a scratch directory.
func serveUnix() string {
	path := filepath.Join(GinkgoT().TempDir(), "skift.sock")
	l, err := net.Listen("unix", path)
	Expect(err).NotTo(HaveOccurred())
	srv := &http.Server{Handler: handler()}
	go func() {
		defer GinkgoRecover()
		if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			Expect(err).NotTo(HaveOccurred())
		}
	}()
	DeferCleanup(srv.Close)
	return path
}

var _ = Describe("Client", Label("unit"), func() {
	Describe("Config", func() {
		var dir string

		BeforeEach(func() {
			dir = GinkgoT().TempDir()
			for _, k := range []string{client.EnvConfig, client.EnvSocket, client.EnvAddress, client.EnvProject} {
				GinkgoT().Setenv(k, "")
			}
			// Point the default config path into scratch so the developer's
			// own file never leaks into a spec.
			GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
		})

		It("is empty when nothing is configured", func() {
			cfg, err := client.LoadConfig("")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg).To(Equal(client.Config{}))
		})

		It("defaults the config path under XDG_CONFIG_HOME, then ~/.config, then nowhere", func() {
			Expect(client.DefaultConfigPath()).To(Equal(filepath.Join(dir, "skift", "config.json")))
			GinkgoT().Setenv("XDG_CONFIG_HOME", "")
			GinkgoT().Setenv("HOME", dir)
			Expect(client.DefaultConfigPath()).To(Equal(filepath.Join(dir, ".config", "skift", "config.json")))
			GinkgoT().Setenv("HOME", "")
			Expect(client.DefaultConfigPath()).To(BeEmpty())
		})

		It("reads the config file at the default path", func() {
			path := filepath.Join(dir, "skift", "config.json")
			Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
			Expect(os.WriteFile(path, []byte(`{"socket": "/tmp/x.sock", "project": "platform"}`), 0o600)).To(Succeed())

			cfg, err := client.LoadConfig("")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg).To(Equal(client.Config{Socket: "/tmp/x.sock", Project: "platform"}))
		})

		It("lays the environment over the file and flags over both", func() {
			path := filepath.Join(dir, "c.json")
			Expect(os.WriteFile(path, []byte(`{"socket": "/from/file", "address": "http://from-file", "project": "file"}`), 0o600)).To(Succeed())
			GinkgoT().Setenv(client.EnvConfig, path)
			GinkgoT().Setenv(client.EnvAddress, "http://skift.tail1234.ts.net")
			GinkgoT().Setenv(client.EnvProject, "env")

			cfg, err := client.LoadConfig("")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg).To(Equal(client.Config{Socket: "/from/file", Address: "http://skift.tail1234.ts.net", Project: "env"}))

			Expect(cfg.Override(client.Config{Project: "flag"})).To(Equal(
				client.Config{Socket: "/from/file", Address: "http://skift.tail1234.ts.net", Project: "flag"}))
		})

		It("fails on an explicit path that does not exist, and on a malformed file", func() {
			_, err := client.LoadConfig(filepath.Join(dir, "missing.json"))
			Expect(err).To(MatchError(os.ErrNotExist))

			path := filepath.Join(dir, "bad.json")
			Expect(os.WriteFile(path, []byte(`{"socket": `), 0o600)).To(Succeed())
			_, err = client.LoadConfig(path)
			Expect(err).To(MatchError(ContainSubstring("parse config")))
		})
	})

	Describe("the default socket", func() {
		It("is the user's own when one exists under XDG_RUNTIME_DIR", func() {
			dir := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_RUNTIME_DIR", dir)
			own := filepath.Join(dir, "skift", "skift.sock")
			Expect(os.MkdirAll(filepath.Dir(own), 0o700)).To(Succeed())
			l, err := net.Listen("unix", own)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(l.Close)

			Expect(client.DefaultSocketPath()).To(Equal(own))
		})

		It("is the system service's when the user has none", func() {
			GinkgoT().Setenv("XDG_RUNTIME_DIR", GinkgoT().TempDir())
			Expect(client.DefaultSocketPath()).To(Equal(client.SystemSocketPath))
		})

		It("is the system service's without a runtime directory at all", func() {
			GinkgoT().Setenv("XDG_RUNTIME_DIR", "")
			Expect(client.DefaultSocketPath()).To(Equal(client.SystemSocketPath))
		})
	})

	Describe("transport selection", func() {
		It("dials the unix socket when no address is configured", func(ctx SpecContext) {
			socket := serveUnix()
			c, err := client.New(client.Config{Socket: socket})
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Transport()).To(Equal(client.TransportUnix))
			Expect(c.Target()).To(Equal(socket))

			res, err := c.Graph.GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "platform"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetProject().GetSlug()).To(Equal("platform"))
		})

		It("dials the address when one is configured, even with a socket set", func(ctx SpecContext) {
			ts := httptest.NewServer(handler())
			DeferCleanup(ts.Close)
			c, err := client.New(client.Config{Socket: "/nonexistent/skift.sock", Address: ts.URL})
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Transport()).To(Equal(client.TransportHTTP))

			res, err := c.Graph.GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "laptop"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Msg.GetProject().GetSlug()).To(Equal("laptop"))
		})

		It("reports an unreachable socket as unavailable, naming the socket", func(ctx SpecContext) {
			socket := filepath.Join(GinkgoT().TempDir(), "nobody.sock")
			c, err := client.New(client.Config{Socket: socket})
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Graph.GetProject(ctx, connect.NewRequest(&skiftv1.GetProjectRequest{Project: "platform"}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).To(ContainSubstring(socket))
		})

		DescribeTable("rejects an address that is not an http or https URL",
			func(address string) {
				_, err := client.New(client.Config{Address: address})
				Expect(err).To(MatchError(ContainSubstring("http or https")))
			},
			Entry("a bare host", "skift.tail1234.ts.net"),
			Entry("another scheme", "unix:///run/skift/skift.sock"),
		)
	})

	Describe("node references", func() {
		DescribeTable("accept the three forms",
			func(in string, want client.NodeRef, canonical string) {
				ref, err := client.ParseNodeRef(in)
				Expect(err).NotTo(HaveOccurred())
				Expect(ref).To(Equal(want))
				Expect(ref.String()).To(Equal(canonical))
			},
			Entry("#7 in the current project", "#7", client.NodeRef{Number: 7}, "#7"),
			Entry("slug#7 in a sibling project", "oauth-spike#7", client.NodeRef{Project: "oauth-spike", Number: 7}, "oauth-spike#7"),
			Entry("org/slug#7 fully qualified", "acme/oauth-spike#7", client.NodeRef{Org: "acme", Project: "oauth-spike", Number: 7}, "acme/oauth-spike#7"),
			Entry("a bare number, as #number", "7", client.NodeRef{Number: 7}, "#7"),
		)

		DescribeTable("reject anything else",
			func(in string) {
				_, err := client.ParseNodeRef(in)
				Expect(err).To(MatchError(client.ErrBadNodeRef))
			},
			Entry("no number", "#"),
			Entry("zero", "#0"),
			Entry("an alias", "spike"),
			Entry("an empty org", "/slug#7"),
			Entry("too many slashes", "a/b/c#7"),
			Entry("whitespace", "slug #7"),
		)

		It("exposes the project half in the form a request accepts", func() {
			Expect(client.NodeRef{Number: 7}.ProjectRef()).To(BeEmpty())
			Expect(client.NodeRef{Project: "p", Number: 7}.ProjectRef()).To(Equal("p"))
			Expect(client.NodeRef{Org: "o", Project: "p", Number: 7}.ProjectRef()).To(Equal("o/p"))
		})
	})

	It("mints request ids that are distinct ULIDs", func() {
		seen := map[string]bool{}
		for range 1000 {
			id := client.NewRequestID()
			Expect(id).To(MatchRegexp(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`))
			Expect(seen[id]).To(BeFalse(), "duplicate id %s", id)
			seen[id] = true
		}
	})
})
