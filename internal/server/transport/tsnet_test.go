package transport_test

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/server"
	"github.com/steading-ai/skift/internal/server/interceptor"
	"github.com/steading-ai/skift/internal/server/transport"
)

var _ = Describe("Tailnet transport", Label("unit"), func() {
	It("is a server transport whose resolver produces the tailscale scheme", func() {
		tn, err := transport.NewTailnet(transport.TailnetConfig{Hostname: "skift"})
		Expect(err).NotTo(HaveOccurred())
		var _ server.Transport = tn
		Expect(tn.Name()).To(Equal("tailnet"))
		Expect(tn.Resolver().Scheme()).To(Equal(interceptor.SchemeTailscale))
	})

	It("requires a hostname and well-formed tags", func() {
		_, err := transport.NewTailnet(transport.TailnetConfig{})
		Expect(err).To(MatchError(ContainSubstring("hostname is required")))

		_, err = transport.NewTailnet(transport.TailnetConfig{Hostname: "skift", Tags: []string{"skift"}})
		Expect(err).To(MatchError(ContainSubstring(`tag "skift" must have the form tag:name`)))

		_, err = transport.NewTailnet(transport.TailnetConfig{Hostname: "skift", Tags: []string{"tag:"}})
		Expect(err).To(MatchError(ContainSubstring("tag:name")))

		_, err = transport.NewTailnet(transport.TailnetConfig{Hostname: "skift", Tags: []string{"tag:skift", "tag:service"}})
		Expect(err).NotTo(HaveOccurred())
	})

	It("marks every accepted connection with its tailnet peer", func(ctx SpecContext) {
		tn, err := transport.NewTailnet(transport.TailnetConfig{Hostname: "skift"})
		Expect(err).NotTo(HaveOccurred())

		l, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(l.Close)
		client, err := net.Dial("tcp", l.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(client.Close)
		conn, err := l.Accept()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(conn.Close)

		peer, ok := interceptor.TailnetPeerFromContext(tn.ConnContext(ctx, conn))
		Expect(ok).To(BeTrue())
		Expect(peer).To(Equal(client.LocalAddr().String()))
	})

	It("cannot say who is calling before it has joined the tailnet", func(ctx SpecContext) {
		tn, err := transport.NewTailnet(transport.TailnetConfig{Hostname: "skift"})
		Expect(err).NotTo(HaveOccurred())
		_, err = tn.WhoIs(ctx, "100.64.0.7:1")
		Expect(err).To(MatchError(ContainSubstring("not up")))
	})
})
