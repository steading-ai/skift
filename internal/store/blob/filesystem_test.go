package blob_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/store/blob"
)

// put uploads and finalizes bytes in one go.
func put(ctx context.Context, b blob.Backend, content string) blob.Sealed {
	GinkgoHelper()
	u, err := b.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	_, err = io.WriteString(u, content)
	Expect(err).NotTo(HaveOccurred())
	sealed, err := u.Finalize(ctx)
	Expect(err).NotTo(HaveOccurred())
	return sealed
}

// get reads the bytes held under digest.
func get(ctx context.Context, b blob.Backend, digest string) string {
	GinkgoHelper()
	r, err := b.Open(ctx, digest)
	Expect(err).NotTo(HaveOccurred())
	defer r.Close() //nolint:errcheck // read-only
	content, err := io.ReadAll(r)
	Expect(err).NotTo(HaveOccurred())
	return string(content)
}

// uploads lists the in-flight upload files under root.
func uploads(root string) []string {
	GinkgoHelper()
	entries, err := os.ReadDir(filepath.Join(root, "uploads"))
	Expect(err).NotTo(HaveOccurred())
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

var _ = Describe("Filesystem backend", Label("unit"), func() {
	var (
		root string
		fs   *blob.Filesystem
	)

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		var err error
		fs, err = blob.NewFilesystem(root)
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.Name()).To(Equal(blob.FilesystemName))
	})

	It("content-addresses finalized bytes by SHA-256", func(ctx SpecContext) {
		sealed := put(ctx, fs, "hello, blob")
		Expect(sealed.Digest).To(Equal(blob.DigestOf([]byte("hello, blob"))))
		Expect(sealed.Size).To(Equal(int64(len("hello, blob"))))
		Expect(sealed.Existed).To(BeFalse())
		Expect(get(ctx, fs, sealed.Digest)).To(Equal("hello, blob"))
		size, err := fs.Stat(ctx, sealed.Digest)
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(Equal(sealed.Size))
		Expect(uploads(root)).To(BeEmpty(), "the upload file moved into place")
	})

	It("holds a finalized blob read-only", func(ctx SpecContext) {
		sealed := put(ctx, fs, "sealed")
		hex := strings.TrimPrefix(sealed.Digest, "sha256:")
		info, err := os.Stat(filepath.Join(root, "blobs", "sha256", hex[:2], hex))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o444)))
	})

	It("stores identical bytes once", func(ctx SpecContext) {
		first := put(ctx, fs, "same bytes")
		second := put(ctx, fs, "same bytes")
		Expect(second.Digest).To(Equal(first.Digest))
		Expect(second.Existed).To(BeTrue())
		Expect(get(ctx, fs, first.Digest)).To(Equal("same bytes"))
		entries, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", "*", "*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(uploads(root)).To(BeEmpty())
	})

	It("reports a digest it does not hold, and refuses one that is not canonical", func(ctx SpecContext) {
		_, err := fs.Open(ctx, blob.DigestOf([]byte("never uploaded")))
		Expect(err).To(MatchError(blob.ErrNotFound))
		_, err = fs.Stat(ctx, blob.DigestOf([]byte("never uploaded")))
		Expect(err).To(MatchError(blob.ErrNotFound))
		_, err = fs.Open(ctx, "sha256:../../etc/passwd")
		Expect(err).To(MatchError(blob.ErrDigest))
	})

	It("takes nothing more after finalize, and nothing after abort", func(ctx SpecContext) {
		u, err := fs.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.WriteString(u, "done")
		Expect(err).NotTo(HaveOccurred())
		_, err = u.Finalize(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.WriteString(u, "more")
		Expect(err).To(MatchError(blob.ErrFinalized))
		_, err = u.Finalize(ctx)
		Expect(err).To(MatchError(blob.ErrFinalized))
		Expect(u.Abort()).To(Succeed())

		aborted, err := fs.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.WriteString(aborted, "partial")
		Expect(err).NotTo(HaveOccurred())
		Expect(aborted.Abort()).To(Succeed())
		Expect(aborted.Abort()).To(Succeed())
		_, err = aborted.Finalize(ctx)
		Expect(err).To(MatchError(blob.ErrAborted))
		Expect(uploads(root)).To(BeEmpty())
		_, err = fs.Open(ctx, blob.DigestOf([]byte("partial")))
		Expect(err).To(MatchError(blob.ErrNotFound))
	})

	It("leaves what is not an upload alone, and reports what stops a reclaim", func(ctx SpecContext) {
		stale, err := fs.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = stale.Abort() })
		Expect(os.Mkdir(filepath.Join(root, "uploads", "not-an-upload"), 0o755)).To(Succeed())

		By("a directory under uploads is not reclaimed")
		n, err := fs.Reclaim(ctx, time.Now().Add(time.Second))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(filepath.Join(root, "uploads", "not-an-upload")).To(BeADirectory())

		By("a cancelled context stops the walk")
		_, err = fs.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = fs.Reclaim(cancelled, time.Now().Add(time.Second))
		Expect(err).To(MatchError(context.Canceled))

		By("and a missing uploads directory is an error, not nothing to do")
		Expect(os.RemoveAll(filepath.Join(root, "uploads"))).To(Succeed())
		_, err = fs.Reclaim(ctx, time.Now())
		Expect(err).To(MatchError(ContainSubstring("list uploads")))
	})

	It("reclaims uploads abandoned before the cutoff and leaves fresh ones", func(ctx SpecContext) {
		stale, err := fs.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.WriteString(stale, "half of the bytes")
		Expect(err).NotTo(HaveOccurred())
		fresh, err := fs.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(uploads(root)).To(HaveLen(2))

		By("nothing is stale yet")
		n, err := fs.Reclaim(ctx, time.Now().Add(-time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))

		By("everything begun before now is reclaimed")
		n, err = fs.Reclaim(ctx, time.Now().Add(time.Second))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2))
		Expect(uploads(root)).To(BeEmpty())

		By("a reclaimed upload cannot finalize")
		_, err = stale.Finalize(ctx)
		Expect(err).To(HaveOccurred())
		_, err = fs.Open(ctx, blob.DigestOf([]byte("half of the bytes")))
		Expect(err).To(MatchError(blob.ErrNotFound))
		Expect(fresh.Abort()).To(Succeed())
	})
})
