package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/server/interceptor"
)

type tlsNodeStub struct {
	network, address string
	listener         net.Listener
	err              error
}

func (n *tlsNodeStub) ListenTLS(network, address string) (net.Listener, error) {
	n.network, n.address = network, address
	return n.listener, n.err
}

var _ = Describe("Native MCP tailnet HTTPS", Label("unit"), func() {
	var parent *Tailnet
	var endpoint *TailnetTLS
	BeforeEach(func() {
		var err error
		parent, err = NewTailnet(TailnetConfig{Hostname: "skift"})
		Expect(err).NotTo(HaveOccurred())
		parent.certificateDNS = "skift.example.ts.net"
		endpoint, err = NewTailnetTLS(parent, 443, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		Expect(err).NotTo(HaveOccurred())
	})
	It("uses the existing enrolled node and supported TLS certificate path", func() {
		Expect(endpoint.node).To(BeIdenticalTo(parent.ts))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		node := &tlsNodeStub{listener: listener}
		endpoint.node = node
		got, err := endpoint.Listen()
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeIdenticalTo(listener))
		Expect(node.network).To(Equal("tcp"))
		Expect(node.address).To(Equal(":443"))
		Expect(endpoint.Close()).To(Succeed())
		// Closing this transport does not close its own listener or shared node.
		probe, err := net.Dial("tcp", listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		Expect(probe.Close()).To(Succeed())
	})
	It("refuses missing certificate configuration and propagates TLS listener failure without downgrade", func() {
		endpoint.node = &tlsNodeStub{err: errors.New("certificate failure")}
		parent.certificateDNS = ""
		_, err := endpoint.Listen()
		Expect(err).To(MatchError(ContainSubstring("MagicDNS and HTTPS")))
		parent.certificateDNS = "skift.example.ts.net"
		_, err = endpoint.Listen()
		Expect(err).To(MatchError(ContainSubstring("certificate failure")))
	})
	It("requires a node, an actual handler and a distinct port", func() {
		_, err := NewTailnetTLS(nil, 443, endpoint.handler)
		Expect(err).To(HaveOccurred())
		_, err = NewTailnetTLS(parent, 443, nil)
		Expect(err).To(HaveOccurred())
		_, err = NewTailnetTLS(parent, 80, endpoint.handler)
		Expect(err).To(MatchError(ContainSubstring("collides")))
	})
	It("retains the original accepted caller peer through TLS", func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		client, err := net.Dial("tcp", listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(client.Close)
		conn, err := listener.Accept()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(conn.Close)
		peer, ok := interceptor.TailnetPeerFromContext(endpoint.ConnContext(context.Background(), tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS12})))
		Expect(ok).To(BeTrue())
		Expect(peer).To(Equal(client.LocalAddr().String()))
	})
	DescribeTable("enforces canonical Host and Origin without proxy trust", func(host string, origins []string, status int) {
		req := httptest.NewRequest(http.MethodPost, "https://skift.example.ts.net/mcp", nil)
		req.Host = host
		for _, origin := range origins {
			req.Header.Add("Origin", origin)
		}
		req.Header.Set("X-Forwarded-Host", "skift.example.ts.net")
		res := httptest.NewRecorder()
		endpoint.HTTPHandler().ServeHTTP(res, req)
		Expect(res.Code).To(Equal(status))
	},
		Entry("native client with no Origin", "skift.example.ts.net", nil, 204),
		Entry("explicit default port", "skift.example.ts.net:443", []string{"https://skift.example.ts.net:443"}, 204),
		Entry("same origin", "skift.example.ts.net", []string{"https://skift.example.ts.net"}, 204),
		Entry("untrusted Host", "evil.example", nil, 403),
		Entry("wrong port", "skift.example.ts.net:8443", nil, 403),
		Entry("cross origin", "skift.example.ts.net", []string{"https://evil.example"}, 403),
		Entry("plaintext origin", "skift.example.ts.net", []string{"http://skift.example.ts.net"}, 403),
		Entry("null origin", "skift.example.ts.net", []string{"null"}, 403),
		Entry("wildcard origin", "skift.example.ts.net", []string{"*"}, 403),
		Entry("empty supplied origin", "skift.example.ts.net", []string{""}, 403),
		Entry("multiple origins", "skift.example.ts.net", []string{"https://skift.example.ts.net", "https://skift.example.ts.net"}, 403),
		Entry("malformed origin", "skift.example.ts.net", []string{"https://skift.example.ts.net/"}, 403),
		Entry("origin with credentials", "skift.example.ts.net", []string{"https://user@skift.example.ts.net"}, 403),
	)
	It("requires the configured non-default port", func() {
		endpoint.port = 8443
		Expect(endpoint.validHost("skift.example.ts.net")).To(BeFalse())
		Expect(endpoint.validHost("skift.example.ts.net:443")).To(BeFalse())
		Expect(endpoint.validHost("skift.example.ts.net:8443")).To(BeTrue())
	})
	It("serves no Connect, health or trailing-slash routes on TLS", func() {
		for _, path := range []string{"/", "/healthz", "/mcp/", "/skift.v1.GraphService/GetProject"} {
			res := httptest.NewRecorder()
			endpoint.HTTPHandler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://skift.example.ts.net"+path, nil))
			Expect(res.Code).To(Equal(http.StatusNotFound))
		}
	})
})
