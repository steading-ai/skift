package server_test

import (
	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/server"
)

var _ = Describe("Wire mapping", Label("unit"), func() {
	It("round-trips page offsets through opaque tokens", func() {
		Expect(server.EncodePageToken(0)).To(BeEmpty())
		token := server.EncodePageToken(40)
		Expect(token).NotTo(ContainSubstring("40"), "opaque")
		Expect(server.DecodePageToken(token)).To(Equal(40))
		Expect(server.DecodePageToken("")).To(Equal(0))

		_, err := server.DecodePageToken("not a token")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

		Expect(server.NextPageToken(0, 10, 10)).To(Equal(server.EncodePageToken(10)))
		Expect(server.NextPageToken(0, 10, 3)).To(BeEmpty(), "a short page is the last")
	})

	It("digests inline text the way the store's blobs are named", func() {
		Expect(server.TextDigest("hello")).To(Equal("sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"))
	})
})
