package skift_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// releasePlatforms are the platforms a release is built for, as they
// appear in asset names.
var releasePlatforms = []string{"linux_amd64", "linux_arm64", "darwin_arm64"}

// releaseVersion is the version the specs build a release as. A release
// candidate's, so the names carry a prerelease part as a rehearsal's do,
// and so the stamp can be found in a binary that cannot run here: no
// dependency carries this string by chance, as several do "9.9.9".
const releaseVersion = "9.9.9-rc.1"

// archiveEntry is one member of a release archive: its header and bytes.
type archiveEntry struct {
	header *tar.Header
	body   []byte
}

// readArchive returns every member of a .tar.gz, in archive order.
func readArchive(path string) []archiveEntry {
	GinkgoHelper()
	f, err := os.Open(path)
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(f.Close()).To(Succeed()) }()
	gz, err := gzip.NewReader(f)
	Expect(err).NotTo(HaveOccurred())

	var entries []archiveEntry
	for r := tar.NewReader(gz); ; {
		header, err := r.Next()
		if err == io.EOF {
			return entries
		}
		Expect(err).NotTo(HaveOccurred())
		body, err := io.ReadAll(r)
		Expect(err).NotTo(HaveOccurred())
		entries = append(entries, archiveEntry{header: header, body: body})
	}
}

// archivedBinary is the skift binary inside a release archive.
func archivedBinary(path string) []byte {
	GinkgoHelper()
	for _, e := range readArchive(path) {
		if e.header.Name == "skift" {
			return e.body
		}
	}
	Fail(path + " holds no skift")
	return nil
}

// sha256File is the hex sha256 of a file's contents.
func sha256File(path string) string {
	GinkgoHelper()
	raw, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// archiveName is the name of a platform's archive in a release.
func archiveName(platform string) string {
	return "skift_" + releaseVersion + "_" + platform + ".tar.gz"
}

// sbomName is the name of a platform's SBOM in a release.
func sbomName(platform string) string {
	return "skift_" + releaseVersion + "_" + platform + ".cdx.json"
}

// archiveSums maps each release archive to its sha256.
func archiveSums(release string) map[string]string {
	GinkgoHelper()
	sums := map[string]string{}
	for _, platform := range releasePlatforms {
		sums[archiveName(platform)] = sha256File(filepath.Join(release, archiveName(platform)))
	}
	return sums
}

// What `make release` leaves in dist/release, which is what the release
// workflow uploads. docs/adrs/011-releases-are-immutable-and-attested.md
// fixes the names.
//
// Serial, because each build compiles for three platforms: beside the
// suite's other specs it starves the ones that run `make lint` against a
// two-minute budget.
var _ = Describe("The release", Label("unit", "slow"), Serial, Ordered, func() {
	var dir, release string

	// The copy gets a commit of its own: the archives take every mtime
	// from it. One build serves every spec but the last, which rebuilds.
	BeforeAll(func() {
		dir = checkout()
		git(dir, "init", "-q")
		git(dir, "add", "-A")
		git(dir, "commit", "-q", "--no-verify", "-m", "initial")
		runIn(dir, 10*time.Minute, "make", "release", "VERSION="+releaseVersion)
		release = filepath.Join(dir, "dist", "release")
	})

	It("builds every platform's archive and SBOM, and nothing else", func() {
		want := []string{"SHA256SUMS"}
		for _, platform := range releasePlatforms {
			want = append(want, archiveName(platform), sbomName(platform))
		}

		entries, err := os.ReadDir(release)
		Expect(err).NotTo(HaveOccurred())
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		Expect(got).To(ConsistOf(want))
	})

	It("puts the binary, the readme and the licence in each archive, owned by root", func() {
		for _, platform := range releasePlatforms {
			var names []string
			for _, e := range readArchive(filepath.Join(release, archiveName(platform))) {
				names = append(names, e.header.Name)
				Expect(e.header.Uid).To(BeZero(), "%s in %s", e.header.Name, platform)
				Expect(e.header.Gid).To(BeZero(), "%s in %s", e.header.Name, platform)
			}
			Expect(names).To(Equal([]string{"LICENSE.md", "README.md", "skift"}), platform)
		}
	})

	It("stamps the archived binary with the version it was built as", func() {
		bin := filepath.Join(GinkgoT().TempDir(), "skift")
		Expect(os.WriteFile(bin,
			archivedBinary(filepath.Join(release, archiveName("linux_"+runtime.GOARCH))), 0o755)).To(Succeed())
		Expect(runIn(dir, time.Minute, bin, "version")).To(Equal("skift version " + releaseVersion))
	})

	// The darwin binary cannot run here, so the stamp is looked for in
	// its bytes.
	It("stamps the darwin binary too", func() {
		darwin := archivedBinary(filepath.Join(release, archiveName("darwin_arm64")))
		Expect(bytes.Contains(darwin, []byte(releaseVersion))).To(BeTrue())
	})

	It("leaves the image the binaries that are in the linux archives", func() {
		for _, arch := range []string{"amd64", "arm64"} {
			staged, err := os.ReadFile(filepath.Join(dir, "dist", "skift-linux-"+arch))
			Expect(err).NotTo(HaveOccurred())
			archived := archivedBinary(filepath.Join(release, archiveName("linux_"+arch)))
			Expect(bytes.Equal(staged, archived)).To(BeTrue(), arch)
		}
	})

	It("catalogues each archive as a CycloneDX SBOM naming its modules", func() {
		for _, platform := range releasePlatforms {
			raw, err := os.ReadFile(filepath.Join(release, sbomName(platform)))
			Expect(err).NotTo(HaveOccurred())
			var bom struct {
				BomFormat  string `json:"bomFormat"`
				Components []struct {
					Name string `json:"name"`
				} `json:"components"`
			}
			Expect(json.Unmarshal(raw, &bom)).To(Succeed(), platform)
			Expect(bom.BomFormat).To(Equal("CycloneDX"), platform)

			var names []string
			for _, c := range bom.Components {
				names = append(names, c.Name)
			}
			Expect(names).To(ContainElement("github.com/spf13/cobra"), platform)
		}
	})

	It("checksums every archive and SBOM in SHA256SUMS", func() {
		f, err := os.Open(filepath.Join(release, "SHA256SUMS"))
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(f.Close()).To(Succeed()) }()

		var listed []string
		for lines := bufio.NewScanner(f); lines.Scan(); {
			sum, name, found := strings.Cut(lines.Text(), "  ")
			Expect(found).To(BeTrue(), "sha256sum format: %q", lines.Text())
			Expect(sha256File(filepath.Join(release, name))).To(Equal(sum), name)
			listed = append(listed, name)
		}

		var want []string
		for _, platform := range releasePlatforms {
			want = append(want, archiveName(platform), sbomName(platform))
		}
		Expect(listed).To(ConsistOf(want))
	})

	It("builds the same archives from the same commit", func() {
		before := archiveSums(release)

		Expect(os.RemoveAll(filepath.Join(dir, "dist"))).To(Succeed())
		runIn(dir, 10*time.Minute, "make", "release", "VERSION="+releaseVersion)

		Expect(archiveSums(release)).To(Equal(before))
	})
})
