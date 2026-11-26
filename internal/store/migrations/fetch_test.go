package migrations_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// releaseDownloads is where each release's archives and SHA256SUMS are
// published, under the release's tag.
const releaseDownloads = "https://github.com/steading-ai/skift/releases/download"

// releaseFetcher fetches a release's skift for this platform, verified
// against the release's SHA256SUMS. It keeps what it downloads in cache,
// one directory per tag, so a later run that finds them there needs no
// network.
type releaseFetcher struct {
	base   string
	cache  string
	client *http.Client
}

// newReleaseFetcher fetches from the published releases into skift's
// directory in the user's cache.
func newReleaseFetcher() (releaseFetcher, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return releaseFetcher{}, fmt.Errorf("no cache directory to keep release archives in: %w", err)
	}
	return releaseFetcher{
		base:   releaseDownloads,
		cache:  filepath.Join(dir, "skift", "releases"),
		client: &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// archiveName is the archive a release publishes for this platform.
func archiveName(tag string) string {
	return fmt.Sprintf("skift_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// binary writes tag's skift into dir and returns its path. The archive
// comes from the cache when the copy there matches the release's
// checksum, and from the release otherwise.
func (f releaseFetcher) binary(ctx context.Context, tag, dir string) (string, error) {
	sums, err := f.cached(ctx, tag, "SHA256SUMS", nil)
	if err != nil {
		return "", err
	}
	want, err := checksumOf(sums, archiveName(tag))
	if err != nil {
		return "", err
	}
	archive, err := f.cached(ctx, tag, archiveName(tag), want)
	if err != nil {
		return "", err
	}
	return extractSkift(archive, filepath.Join(dir, "skift-"+tag))
}

// cached returns the path of tag's file name in the cache. It downloads
// the file first when the cache lacks it or, given want, holds a copy
// whose SHA-256 differs. A download replaces the cached copy only once it
// verifies, so a run that is interrupted or refused leaves none behind.
func (f releaseFetcher) cached(ctx context.Context, tag, name string, want []byte) (string, error) {
	path := filepath.Join(f.cache, tag, name)
	if got, err := sha256File(path); err == nil && (want == nil || bytes.Equal(got, want)) {
		return path, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), name+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone already once renamed into place
	url := f.base + "/" + tag + "/" + name
	got, err := f.download(ctx, url, tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	if want != nil && !bytes.Equal(got, want) {
		return "", fmt.Errorf("fetch %s: its SHA-256 is %x, and SHA256SUMS says %x", url, got, want)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// download writes what url serves to w and returns its SHA-256.
func (f releaseFetcher) download(ctx context.Context, url string, w io.Writer) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() //nolint:errcheck // response body of a finished read
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", res.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), res.Body); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// sha256File is the SHA-256 of the file at path.
func sha256File(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read only
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// checksumOf reads name's SHA-256 from the SHA256SUMS file at path, whose
// lines are a hex digest, two spaces, and a file name.
func checksumOf(path, name string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read only
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		digest, listed, ok := strings.Cut(scanner.Text(), "  ")
		if ok && listed == name {
			return hex.DecodeString(digest)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s lists no %s", path, name)
}

// extractSkift writes the skift binary in the archive at path to dest.
func extractSkift(path, dest string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // read only
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s holds no skift binary", path)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Clean(header.Name) != "skift" {
			continue
		}
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, archive); err != nil { //nolint:gosec // a release archive this run verified
			out.Close() //nolint:errcheck,gosec // the copy's error is the one to report
			return "", err
		}
		return dest, out.Close()
	}
}

var _ = Describe("Fetching a release's binary", Label("unit"), func() {
	const tag = "v9.8.7"
	var (
		binaryBytes []byte
		archive     []byte
		sums        string
		requests    atomic.Int64
		server      *httptest.Server
		fetcher     releaseFetcher
	)

	BeforeEach(func() {
		binaryBytes = []byte("#!/bin/sh\necho skift 9.8.7\n")
		archive = tarGz(map[string][]byte{"README.md": []byte("read me"), "skift": binaryBytes})
		digest := sha256.Sum256(archive)
		sums = fmt.Sprintf("%x  %s\n%x  skift_9.8.7_plan9_mips.tar.gz\n", digest, archiveName(tag), sha256.Sum256(nil))
		requests.Store(0)
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			switch r.URL.Path {
			case "/" + tag + "/SHA256SUMS":
				_, _ = io.WriteString(w, sums)
			case "/" + tag + "/" + archiveName(tag):
				_, _ = w.Write(archive)
			default:
				http.NotFound(w, r)
			}
		}))
		DeferCleanup(server.Close)
		fetcher = releaseFetcher{base: server.URL, cache: GinkgoT().TempDir(), client: server.Client()}
	})

	It("downloads the archive once, verified, and needs no network while the cache holds it", func(ctx SpecContext) {
		By("given an empty cache, it fetches the checksums and the archive")
		path, err := fetcher.binary(ctx, tag, GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(os.ReadFile(path)).To(Equal(binaryBytes))
		Expect(requests.Load()).To(BeNumerically("==", 2))

		By("and with the release unreachable, the cache alone serves the next run")
		server.Close()
		path, err = fetcher.binary(ctx, tag, GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(os.ReadFile(path)).To(Equal(binaryBytes))
		Expect(requests.Load()).To(BeNumerically("==", 2))
	})

	It("refuses an archive whose checksum differs from the release's, and caches nothing of it", func(ctx SpecContext) {
		archive = append(archive, 0)
		_, err := fetcher.binary(ctx, tag, GinkgoT().TempDir())
		Expect(err).To(MatchError(ContainSubstring("SHA256SUMS says")))
		Expect(filepath.Join(fetcher.cache, tag, archiveName(tag))).NotTo(BeAnExistingFile())
	})

	It("fetches the archive again when the cached copy no longer matches", func(ctx SpecContext) {
		_, err := fetcher.binary(ctx, tag, GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(fetcher.cache, tag, archiveName(tag)), []byte("damaged"), 0o644)).To(Succeed())

		path, err := fetcher.binary(ctx, tag, GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(os.ReadFile(path)).To(Equal(binaryBytes))
		Expect(requests.Load()).To(BeNumerically("==", 3))
	})

	It("names a release that published no archive for this platform", func(ctx SpecContext) {
		sums = "0000  skift_9.8.7_plan9_mips.tar.gz\n"
		_, err := fetcher.binary(ctx, tag, GinkgoT().TempDir())
		Expect(err).To(MatchError(ContainSubstring("lists no " + archiveName(tag))))
	})
})

// tarGz builds a gzipped tar archive of files, as a release publishes.
func tarGz(files map[string][]byte) []byte {
	GinkgoHelper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		Expect(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})).To(Succeed())
		_, err := tw.Write(body)
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(tw.Close()).To(Succeed())
	Expect(gz.Close()).To(Succeed())
	return buf.Bytes()
}
