package domain_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing/iotest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/store/blob"
	"github.com/ivy/dagger/internal/testsupport"
)

// gitCommit is a full SHA-1 commit hash, as a git reference records.
const gitCommit = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// attach is an attach_artifact operation.
func attach(node string, artifact domain.Artifact) domain.Op {
	return domain.Op{AttachArtifact: &domain.AttachArtifact{Node: node, Artifact: artifact}}
}

// blobArtifact is an artifact named "bytes" of one managed blob part.
func blobArtifact(digest string) domain.Artifact {
	return domain.Artifact{Name: "bytes", Parts: []domain.Part{{Blob: &domain.BlobPart{Digest: digest}}}}
}

// referenceArtifact is an artifact of one reference part carrying the
// spec digest.
func referenceArtifact(name, uri string) domain.Artifact {
	return domain.Artifact{Name: name, Parts: []domain.Part{{Reference: &domain.ReferencePart{URI: uri, Digest: specDigest}}}}
}

// upload streams content into the backend as the spec's organization.
func (g *graph) upload(ctx context.Context, backend blob.Backend, content string) (store.Blob, error) {
	GinkgoHelper()
	return domain.UploadBlob(ctx, g.s, backend, domain.BlobUpload{OrgID: g.org, MediaType: "text/plain", Body: strings.NewReader(content)})
}

func (g *graph) mustUpload(ctx context.Context, backend blob.Backend, content string) store.Blob {
	GinkgoHelper()
	b, err := g.upload(ctx, backend, content)
	Expect(err).NotTo(HaveOccurred())
	return b
}

// readBlob reads a blob with its reference count, in a transaction of
// its own.
func (g *graph) readBlob(ctx context.Context, digest string) (*domain.BlobInfo, error) {
	GinkgoHelper()
	var info *domain.BlobInfo
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
		info, err = domain.ReadBlob(ctx, tx, g.org, digest)
		return err
	})
	return info, err
}

// openBlob reads a blob's bytes as org, in a transaction of its own.
func (g *graph) openBlob(ctx context.Context, backend blob.Backend, org, digest string) (string, error) {
	GinkgoHelper()
	var content []byte
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, r, err := domain.OpenBlob(ctx, tx, backend, org, digest)
		if err != nil {
			return err
		}
		defer r.Close() //nolint:errcheck // read-only
		content, err = io.ReadAll(r)
		return err
	})
	return string(content), err
}

// artifacts reads every artifact of a node as its downstream would see
// them: by making the node block another and fetching that one's inputs.
func (g *graph) artifactsOf(ctx context.Context, ref string) []domain.StoredArtifact {
	GinkgoHelper()
	all, err := g.s.Queries().ListArtifactsForNode(ctx, store.ListArtifactsForNodeParams{OrgID: g.org, NodeID: g.id(ref)})
	Expect(err).NotTo(HaveOccurred())
	out := make([]domain.StoredArtifact, 0, len(all))
	for _, a := range all {
		parts, err := g.s.Queries().ListArtifactParts(ctx, store.ListArtifactPartsParams{OrgID: g.org, ArtifactID: a.ID})
		Expect(err).NotTo(HaveOccurred())
		stored := domain.StoredArtifact{Artifact: a}
		for _, p := range parts {
			stored.Parts = append(stored.Parts, domain.StoredPart{ArtifactPart: p})
		}
		out = append(out, stored)
	}
	return out
}

// storedObjects counts the objects a filesystem backend holds.
func storedObjects(root string) int {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", "*", "*"))
	Expect(err).NotTo(HaveOccurred())
	return len(files)
}

var _ = Describe("Artifacts", Label(testsupport.DBLabel), func() {
	var (
		s       *store.Store
		g       *graph
		actor   domain.Actor
		root    string
		backend *blob.Filesystem
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
		actor = g.principal(ctx)
		root = GinkgoT().TempDir()
		var err error
		backend, err = blob.NewFilesystem(root)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("Scenarios", func() {
		It("A design agent hands work to a coding agent", func(ctx SpecContext) {
			By("given an agent completes a node attaching an OCI reference with a digest")
			g.mustApply(ctx, actor, "setup",
				create("design", "Design the widget"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "code", Title: "Build the widget", BlockedBy: []string{"design"}}},
			)
			bundle := []byte("PNG and markdown, pushed with oras")
			digest := blob.DigestOf(bundle)
			designer := g.agent(ctx)
			claimed := g.mustClaim(ctx, designer, claimReq("claim-design", time.Minute))
			Expect(claimed.Node.Number).To(Equal(int64(1)))
			uri := "oci://registry.example.com/acme/widget-design@" + digest
			_, err := g.applyWithLease(ctx, designer, "done-design", claimed.Lease.ID,
				complete("#1", "# Design\n\nPushed the bundle.", domain.Artifact{
					Name: "design", Description: "the design bundle",
					Parts: []domain.Part{{Reference: &domain.ReferencePart{URI: uri, Digest: digest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: int64(len(bundle))}}},
				}))
			Expect(err).NotTo(HaveOccurred())

			By("when a downstream agent fetches its inputs")
			in := g.inputs(ctx, "#2")

			By("then the reference and digest are returned")
			Expect(in).To(HaveLen(1))
			Expect(in[0].Artifacts).To(HaveLen(1))
			Expect(in[0].Artifacts[0].Name).To(Equal("design"))
			Expect(in[0].Artifacts[0].CreatedBy).To(Equal(designer.PrincipalID))
			Expect(in[0].Artifacts[0].Parts).To(HaveLen(1))
			part := in[0].Artifacts[0].Parts[0]
			Expect(part.Kind).To(Equal(domain.PartReference))
			Expect(part.ReferenceUri).To(HaveValue(Equal(uri)))
			Expect(part.ReferenceDigest).To(HaveValue(Equal(digest)))
			Expect(part.Untrusted).To(BeTrue(), "the URI is agent-supplied input")

			By("and the caller can verify the bytes it pulls")
			pulled := bundle
			Expect(blob.DigestOf(pulled)).To(Equal(*part.ReferenceDigest))
			Expect(blob.DigestOf([]byte("something else at that URI"))).NotTo(Equal(*part.ReferenceDigest))
		})

		It("Deduplication by content", func(ctx SpecContext) {
			By("given two nodes attaching identical bytes")
			g.mustApply(ctx, actor, "setup", create("one", "One"), create("two", "Two"))
			first := g.mustUpload(ctx, backend, "the same bytes")
			second := g.mustUpload(ctx, backend, "the same bytes")
			Expect(second.Digest).To(Equal(first.Digest))
			Expect(second.CreatedAt).To(Equal(first.CreatedAt), "the second upload returned the first row")
			g.mustApply(ctx, actor, "attach", attach("#1", blobArtifact(first.Digest)), attach("#2", blobArtifact(first.Digest)))

			By("then one blob row exists with a reference count of two")
			info, err := g.readBlob(ctx, first.Digest)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.References).To(Equal(int64(2)))
			Expect(info.Size).To(Equal(int64(len("the same bytes"))))
			Expect(storedObjects(root)).To(Equal(1), "the backend holds the bytes once")
			for _, ref := range []string{"#1", "#2"} {
				arts := g.artifactsOf(ctx, ref)
				Expect(arts).To(HaveLen(1))
				Expect(arts[0].Parts[0].BlobDigest).To(HaveValue(Equal(first.Digest)))
				Expect(arts[0].Parts[0].MediaType).To(HaveValue(Equal("text/plain")))
				Expect(arts[0].Parts[0].Size).To(HaveValue(Equal(int64(len("the same bytes")))))
			}
		})

		It("An interrupted upload attaches nothing", func(ctx SpecContext) {
			By("given an upload that fails before finalize")
			g.mustApply(ctx, actor, "setup", create("one", "One"))
			content := "the whole of the bytes"
			digest := blob.DigestOf([]byte(content))
			broken := io.MultiReader(strings.NewReader(content[:8]), iotest.ErrReader(errors.New("connection reset")))
			_, err := domain.UploadBlob(ctx, s, backend, domain.BlobUpload{OrgID: g.org, Body: broken})
			Expect(err).To(MatchError(ContainSubstring("connection reset")))

			By("then no artifact references it")
			_, err = g.readBlob(ctx, digest)
			Expect(err).To(MatchError(domain.ErrBlobNotFound))
			_, err = g.readBlob(ctx, blob.DigestOf([]byte(content[:8])))
			Expect(err).To(MatchError(domain.ErrBlobNotFound), "the partial bytes are not a blob either")
			_, err = g.apply(ctx, actor, "attach", attach("#1", blobArtifact(digest)))
			Expect(err).To(MatchError(domain.ErrBlobNotFound))
			Expect(g.artifactsOf(ctx, "#1")).To(BeEmpty())
			Expect(storedObjects(root)).To(BeZero())

			By("and the partial blob is reclaimable")
			// A process that dies mid-upload aborts nothing; what it left
			// behind is the backend's to sweep.
			crashed, err := backend.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = io.WriteString(crashed, content[:8])
			Expect(err).NotTo(HaveOccurred())
			reclaimed, err := backend.Reclaim(ctx, time.Now().Add(time.Second))
			Expect(err).NotTo(HaveOccurred())
			Expect(reclaimed).To(Equal(1))
			_, err = crashed.Finalize(ctx)
			Expect(err).To(HaveOccurred(), "a reclaimed upload cannot finalize")
			_, err = backend.Open(ctx, blob.DigestOf([]byte(content[:8])))
			Expect(err).To(MatchError(blob.ErrNotFound))
		})
	})

	Describe("Managed blobs", func() {
		It("are content-addressed by SHA-256 and immutable", func(ctx SpecContext) {
			b := g.mustUpload(ctx, backend, "immutable bytes")
			Expect(b.Digest).To(Equal(blob.DigestOf([]byte("immutable bytes"))))
			Expect(b.Backend).To(Equal(backend.Name()))
			Expect(b.MediaType).To(Equal("text/plain"))

			content, err := g.openBlob(ctx, backend, g.org, b.Digest)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).To(Equal("immutable bytes"))

			By("uploading the same bytes again under another media type changes nothing")
			again, err := domain.UploadBlob(ctx, s, backend, domain.BlobUpload{OrgID: g.org, MediaType: "application/octet-stream", Body: strings.NewReader("immutable bytes")})
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(b))
			Expect(storedObjects(root)).To(Equal(1))
		})

		It("refuse bytes that do not hash to the digest the caller expected", func(ctx SpecContext) {
			expected := blob.DigestOf([]byte("what I meant to send"))
			_, err := domain.UploadBlob(ctx, s, backend, domain.BlobUpload{OrgID: g.org, Digest: expected, Body: strings.NewReader("what I actually sent")})
			Expect(err).To(MatchError(domain.ErrDigestMismatch))
			_, err = g.readBlob(ctx, expected)
			Expect(err).To(MatchError(domain.ErrBlobNotFound))
			_, err = g.readBlob(ctx, blob.DigestOf([]byte("what I actually sent")))
			Expect(err).To(MatchError(domain.ErrBlobNotFound), "nothing is recorded under the real digest either")
		})

		It("refuse a body over the blob ceiling before recording anything", func(ctx SpecContext) {
			small := domain.DefaultLimits()
			small.BlobBytes = 16
			Expect(domain.SetLimits(small)).To(Succeed())
			DeferCleanup(func() { Expect(domain.SetLimits(domain.DefaultLimits())).To(Succeed()) })

			_, err := g.upload(ctx, backend, strings.Repeat("x", 16))
			Expect(err).NotTo(HaveOccurred(), "at the ceiling is allowed")
			_, err = g.upload(ctx, backend, strings.Repeat("x", 17))
			limitErr := asLimitError(err, domain.LimitBlobBytes)
			Expect(limitErr.Max).To(Equal(int64(16)))
			_, err = g.readBlob(ctx, blob.DigestOf([]byte(strings.Repeat("x", 17))))
			Expect(err).To(MatchError(domain.ErrBlobNotFound))
			Expect(storedObjects(root)).To(Equal(1))
		})

		It("are scoped to the organization that uploaded them", func(ctx SpecContext) {
			b := g.mustUpload(ctx, backend, "ours")
			var other string
			Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				org, err := tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: newULID(), Name: "rival"})
				other = org.ID
				return err
			})).To(Succeed())
			_, err := g.openBlob(ctx, backend, other, b.Digest)
			Expect(err).To(MatchError(domain.ErrBlobNotFound), "the backend holds the bytes, but the row is not theirs")
			content, err := g.openBlob(ctx, backend, g.org, b.Digest)
			Expect(err).NotTo(HaveOccurred())
			Expect(content).To(Equal("ours"))
		})
	})

	Describe("References", func() {
		It("record the resolved commit of a git reference, not just the branch", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("one", "One"))
			for _, uri := range []string{
				"git+https://example.com/acme/platform.git?ref=refs/heads/main",
				"git+ssh://git@example.com/acme/platform.git?ref=main&rev=4b825dc",
				"git://example.com/acme/platform.git",
				"GIT+HTTPS://example.com/acme/platform/pull/7",
			} {
				_, err := g.apply(ctx, actor, "branch-only", attach("#1", referenceArtifact("branch", uri)))
				Expect(err).To(MatchError(domain.ErrGitCommitRequired), uri)
			}
			Expect(g.artifactsOf(ctx, "#1")).To(BeEmpty())

			resolved := "git+https://example.com/acme/platform.git?ref=refs/heads/main&rev=" + gitCommit
			g.mustApply(ctx, actor, "resolved", attach("#1", referenceArtifact("branch", resolved)))
			arts := g.artifactsOf(ctx, "#1")
			Expect(arts).To(HaveLen(1))
			Expect(arts[0].Parts[0].ReferenceUri).To(HaveValue(Equal(resolved)))
		})

		It("are stored verbatim, never fetched, and returned marked as untrusted input", func(ctx SpecContext) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			DeferCleanup(server.Close)
			uri := server.URL + "/acme/report.tar.gz?token=SECRET%20VALUE&x=%2F..%2F"

			g.mustApply(ctx, actor, "setup",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", BlockedBy: []string{"one"}}},
			)
			g.mustApply(ctx, actor, "attach", attach("#1", referenceArtifact("report", uri)))
			in := g.inputs(ctx, "#2")

			Expect(hits.Load()).To(BeZero(), "dagger never fetched the URI")
			Expect(in).To(HaveLen(1))
			Expect(in[0].Artifacts).To(HaveLen(1))
			part := in[0].Artifacts[0].Parts[0]
			Expect(part.ReferenceUri).To(HaveValue(Equal(uri)))
			Expect(part.Untrusted).To(BeTrue())
			Expect(part.ReferenceDigest).To(HaveValue(Equal(specDigest)))
		})

		It("mark only reference parts as untrusted", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup",
				create("one", "One"),
				domain.Op{CreateNode: &domain.CreateNode{Alias: "two", Title: "Two", BlockedBy: []string{"one"}}},
			)
			b := g.mustUpload(ctx, backend, "managed")
			g.mustApply(ctx, actor, "attach", attach("#1", domain.Artifact{Name: "mixed", Parts: []domain.Part{
				{Text: &domain.TextPart{Text: "inline"}},
				{Blob: &domain.BlobPart{Digest: b.Digest}},
				{Reference: &domain.ReferencePart{URI: "https://example.com/x", Digest: specDigest}},
			}}))
			parts := g.inputs(ctx, "#2")[0].Artifacts[0].Parts
			Expect(parts).To(HaveLen(3))
			Expect(parts[0].Kind).To(Equal(domain.PartText))
			Expect(parts[0].Untrusted).To(BeFalse())
			Expect(parts[1].Kind).To(Equal(domain.PartBlob))
			Expect(parts[1].Untrusted).To(BeFalse())
			Expect(parts[2].Kind).To(Equal(domain.PartReference))
			Expect(parts[2].Untrusted).To(BeTrue())
		})
	})

	Describe("attach_artifact", func() {
		It("attaches to a node, writes an event, and keeps names unique within the node", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("one", "One"))
			b := g.mustUpload(ctx, backend, "attached later")
			g.mustApply(ctx, actor, "attach", attach("#1", blobArtifact(b.Digest)))
			Expect(ops(g.nodeEvents(ctx, "#1"))).To(ContainElement(domain.OpAttachArtifact))

			_, err := g.apply(ctx, actor, "again", attach("#1", blobArtifact(b.Digest)))
			Expect(err).To(MatchError(domain.ErrArtifactNameTaken))
			Expect(g.artifactsOf(ctx, "#1")).To(HaveLen(1))
			info, err := g.readBlob(ctx, b.Digest)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.References).To(Equal(int64(1)))
		})

		It("is fenced on a held node, and an agent's only on its own", func(ctx SpecContext) {
			g.mustApply(ctx, actor, "setup", create("one", "One"), create("two", "Two"))
			holder, bystander := g.agent(ctx), g.agent(ctx)
			claimed := g.mustClaim(ctx, holder, claimReq("claim", time.Minute))
			Expect(claimed.Node.Number).To(Equal(int64(1)))

			By("a write to the held node carries its lease")
			_, err := g.apply(ctx, actor, "unfenced", attach("#1", referenceArtifact("r", "https://example.com/a")))
			Expect(err).To(MatchError(domain.ErrLeaseRequired))

			By("an agent attaches only to its own node")
			_, err = g.apply(ctx, bystander, "not-mine", attach("#2", referenceArtifact("r", "https://example.com/a")))
			Expect(asDenial(err)).NotTo(BeNil(), "got %v", err)
			_, err = g.applyWithLease(ctx, holder, "mine", claimed.Lease.ID, attach("#1", referenceArtifact("r", "https://example.com/a")))
			Expect(err).NotTo(HaveOccurred())
			Expect(g.artifactsOf(ctx, "#1")).To(HaveLen(1))
			Expect(g.artifactsOf(ctx, "#2")).To(BeEmpty())
		})
	})
})
