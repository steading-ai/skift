package domain_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

var _ = Describe("Artifact validation", Label(testsupport.DBLabel), func() {
	digest := "sha256:" + strings.Repeat("ab", 32)
	commit := strings.Repeat("c", 40)

	It("refuses a part that is not exactly one thing, a digest out of shape, and a git reference with no commit", func(ctx SpecContext) {
		s := store.FromPool(pg.Database(ctx))
		g := newGraph(ctx, s)
		user := g.principal(ctx)
		g.given(ctx, func(ctx context.Context, tx store.Tx) { g.node(ctx, tx, 1, nil) })

		attach := func(parts ...domain.Part) error {
			GinkgoHelper()
			_, err := g.apply(ctx, user, "attach-"+strings.Repeat("x", len(parts)), domain.Op{AttachArtifact: &domain.AttachArtifact{
				Node: "#1", Artifact: domain.Artifact{Name: "notes", Parts: parts},
			}})
			return err
		}

		Expect(attach(domain.Part{})).To(MatchError(ContainSubstring("exactly one of text, blob, or reference")))
		Expect(attach(domain.Part{Text: &domain.TextPart{Text: "a"}, Blob: &domain.BlobPart{Digest: digest}})).
			To(MatchError(ContainSubstring("exactly one of text, blob, or reference")))
		Expect(attach(domain.Part{Blob: &domain.BlobPart{Digest: "abc"}})).To(MatchError(ContainSubstring(`blob digest "abc" is not sha256:<hex>`)))
		Expect(attach(domain.Part{Reference: &domain.ReferencePart{Digest: digest}})).To(MatchError(ContainSubstring("a reference needs a uri")))
		Expect(attach(domain.Part{Reference: &domain.ReferencePart{URI: "https://example.com/x", Digest: "nope"}})).
			To(MatchError(ContainSubstring(`reference digest "nope" is not sha256:<hex>`)))
		Expect(attach(domain.Part{Reference: &domain.ReferencePart{URI: "https://example.com/x", Digest: digest, Size: -1}})).
			To(MatchError(ContainSubstring("reference size is negative")))
		Expect(attach(domain.Part{Reference: &domain.ReferencePart{URI: "git+https://github.com/acme/x.git", Digest: digest}})).
			To(MatchError(domain.ErrGitCommitRequired))
		Expect(attach(domain.Part{Reference: &domain.ReferencePart{URI: "GIT+SSH://git@github.com/acme/x.git?rev=main", Digest: digest}})).
			To(MatchError(domain.ErrGitCommitRequired), "the scheme is read case-insensitively, and a branch is not a commit")
		Expect(g.artifactsOf(ctx, "#1")).To(BeEmpty(), "a refused patch attached nothing")

		By("and takes a git reference that records the commit it resolved to, and a plain URI as given")
		Expect(attach(
			domain.Part{Reference: &domain.ReferencePart{URI: "git+https://github.com/acme/x.git?rev=" + commit, Digest: digest, Size: 12}},
			domain.Part{Reference: &domain.ReferencePart{URI: "s3://bucket/key", Digest: digest, MediaType: "text/plain"}},
		)).To(Succeed())
		attached := g.artifactsOf(ctx, "#1")
		Expect(attached).To(HaveLen(1))
		Expect(attached[0].Parts).To(HaveLen(2))

		By("storing a size only when the reference gave one, since zero would claim the target is empty")
		Expect(attached[0].Parts[0].Size).To(HaveValue(Equal(int64(12))))
		Expect(attached[0].Parts[1].Size).To(BeNil())
		Expect(attached[0].Parts[1].MediaType).To(HaveValue(Equal("text/plain")))
	})
})
