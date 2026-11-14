package skift_test

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// agentDir holds this repository's own agent configuration, and
// skillsFolder is the name of a directory of agent skills wherever one
// sits. An agent reads those files, and a link there that shows a file's
// path is an instruction about which file to open.
const (
	agentDir     = ".claude/"
	skillsFolder = "skills"
)

// readerPages keeps the Markdown pages a person reads: those outside the
// archive, outside agentDir, and outside every skills folder.
func readerPages(files []string) []string {
	var kept []string
	for _, name := range markdown(current(files)) {
		if strings.HasPrefix(name, agentDir) || slices.Contains(strings.Split(path.Dir(name), "/"), skillsFolder) {
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

// shownAddress is a place where a page shows an address instead of
// naming what it leads to: the line, and what is written there.
type shownAddress struct {
	line int
	what string
}

var (
	// A link with its text, inline or by reference, and the mark that
	// makes it an image. The text may hold code spans.
	textedLink = regexp.MustCompile("(!?)\\[((?:`[^`]*`|[^\\]`])*)\\](\\([^)]*\\)|\\[[^\\]]*\\])")
	pagePath   = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.md(?:#\S*)?$`)
	webURL     = regexp.MustCompile(`https?://[^\s<>\[\]"']+`)
	onlyWebURL = regexp.MustCompile(`^https?://\S+$`)
	autolink   = regexp.MustCompile(`<https?://[^\s<>]+>`)
)

// codeBlanked overwrites every code span of a line with spaces, backticks
// included, so that what is left keeps the offsets it has in the line.
func codeBlanked(line string) []byte {
	out := []byte(line)
	for at := 0; at < len(line); {
		start := strings.IndexByte(line[at:], '`')
		if start < 0 {
			break
		}
		start += at
		ticks := len(line[start:]) - len(strings.TrimLeft(line[start:], "`"))
		end := strings.Index(line[start+ticks:], line[start:start+ticks])
		if end < 0 {
			break
		}
		at = start + ticks + end + ticks
		blank(out, start, at)
	}
	return out
}

// blank overwrites a stretch of a line with spaces.
func blank(line []byte, from, to int) {
	for i := from; i < to; i++ {
		line[i] = ' '
	}
}

// scanLinkText reads one Markdown file's source for addresses shown to
// the reader: links whose text is the path of a Markdown file, and URLs
// written as themselves, which is an autolink, a link whose text is a
// URL, or a URL in running text. Front matter and code, fenced or
// inline, are not prose: a URL there is a value, and is left alone. So is
// the destination of a link, a definition, an image or an HTML attribute.
func scanLinkText(source string) (paths, urls []shownAddress) {
	for _, line := range proseLines(source) {
		prose := codeBlanked(line.text)
		for _, m := range textedLink.FindAllStringSubmatchIndex(line.text, -1) {
			if prose[m[0]] == ' ' {
				continue // the brackets are inside a code span
			}
			blank(prose, m[6], m[7])
			if m[2] != m[3] {
				continue // an image's text describes it, and is never shown as a link
			}
			link := line.text[m[0]:m[1]]
			text := strings.Trim(strings.TrimSpace(line.text[m[4]:m[5]]), "`")
			switch {
			case pagePath.MatchString(text):
				paths = append(paths, shownAddress{line.number, link})
			case onlyWebURL.MatchString(text):
				urls = append(urls, shownAddress{line.number, link})
				blank(prose, m[4], m[5])
			}
		}
		for _, re := range []*regexp.Regexp{definition, htmlTarget} {
			for _, m := range re.FindAllSubmatchIndex(prose, -1) {
				blank(prose, m[2], m[3])
			}
		}
		for _, m := range autolink.FindAllIndex(prose, -1) {
			urls = append(urls, shownAddress{line.number, string(prose[m[0]:m[1]])})
			blank(prose, m[0], m[1])
		}
		for _, found := range webURL.FindAll(prose, -1) {
			urls = append(urls, shownAddress{line.number, strings.TrimRight(string(found), ".,;:!?)")})
		}
	}
	return paths, urls
}

var _ = Describe("Documentation link text", Label("unit"), func() {
	// shown reads every page a person reads, and lists what scanLinkText
	// finds in each as file:line: what is written.
	shown := func() (paths, urls []string) {
		GinkgoHelper()
		pages := readerPages(trackedFiles())
		Expect(pages).To(ContainElements("README.md", "SECURITY.md", "plugin/skift/README.md"))
		Expect(pages).To(ContainElement(HavePrefix("docs/adrs/")))
		for _, page := range pages {
			raw, err := os.ReadFile(page)
			Expect(err).NotTo(HaveOccurred())
			pagePaths, pageURLs := scanLinkText(string(raw))
			for _, found := range pagePaths {
				paths = append(paths, fmt.Sprintf("%s:%d: %s", page, found.line, found.what))
			}
			for _, found := range pageURLs {
				urls = append(urls, fmt.Sprintf("%s:%d: %s", page, found.line, found.what))
			}
		}
		return paths, urls
	}

	It("No page names another by its file path", func() {
		By("given every tracked Markdown page outside archive, .claude and the skills directories")
		By("when the text of each link is read")
		paths, _ := shown()

		By("then none is the path of a Markdown file")
		Expect(paths).To(BeEmpty(), "links whose text is a Markdown file's path: name the page the link leads to")
	})

	It("No page shows a bare URL", func() {
		By("given every tracked Markdown page outside archive, .claude and the skills directories")
		By("when its prose is read, leaving out code")
		_, urls := shown()

		By("then no URL is an autolink, a link's own text, or written into a sentence")
		Expect(urls).To(BeEmpty(), "URLs written as themselves: make each a link whose text names what it leads to")
	})

	Describe("choosing the pages a person reads", func() {
		It("leaves out the archive, the agent configuration and every skills directory", func() {
			Expect(readerPages([]string{
				"README.md",
				"AGENTS.md",
				"docs/adrs/001-a-decision.md",
				"plugin/skift/README.md",
				"plugin/skift/skills/skift/SKILL.md",
				"plugin/skift/skills/skift/references/commands.md",
				".claude/skills/harden-tests/SKILL.md",
				".claude/notes.md",
				"archive/README.md",
				"docs/skills.md",
				"site.yaml",
			})).To(Equal([]string{
				"README.md",
				"AGENTS.md",
				"docs/adrs/001-a-decision.md",
				"plugin/skift/README.md",
				"docs/skills.md",
			}))
		})
	})

	Describe("reading link text", func() {
		// found is what scanLinkText reports for a page, as line: what.
		found := func(lines ...string) (paths, urls []string) {
			pagePaths, pageURLs := scanLinkText(strings.Join(lines, "\n"))
			for _, f := range pagePaths {
				paths = append(paths, fmt.Sprintf("%d: %s", f.line, f.what))
			}
			for _, f := range pageURLs {
				urls = append(urls, fmt.Sprintf("%d: %s", f.line, f.what))
			}
			return paths, urls
		}

		It("reports a link whose text is a Markdown file's path, by line", func() {
			paths, urls := found(
				"See [docs/vision.md](docs/vision.md) for why.",
				"[`docs/ci.md`](docs/ci.md#dependency-updates) describes that, and [SECURITY.md](SECURITY.md) the policy.",
				"[../AGENTS.md#how-to-work-here](../AGENTS.md#how-to-work-here), [by reference.md][ref] and [notes.md][ref].",
			)
			Expect(paths).To(Equal([]string{
				"1: [docs/vision.md](docs/vision.md)",
				"2: [`docs/ci.md`](docs/ci.md#dependency-updates)",
				"2: [SECURITY.md](SECURITY.md)",
				"3: [../AGENTS.md#how-to-work-here](../AGENTS.md#how-to-work-here)",
				"3: [notes.md][ref]",
			}))
			Expect(urls).To(BeEmpty())
		})

		It("passes a link that names its page, and one whose text is a file that is no page", func() {
			paths, urls := found(
				"---",
				"see: [docs/front-matter.md](docs/front-matter.md)",
				"---",
				"See the [vision](docs/vision.md), and [How to cut a release](docs/releasing.md).",
				"[check.yml](../.github/workflows/check.yml) and [site.yaml](../site.yaml) are what they are called.",
				"Write `[docs/vision.md](docs/vision.md)` and the spec fails.",
				"![docs/shot.md](shot.png)",
				"```markdown",
				"[docs/fenced.md](docs/fenced.md)",
				"```",
			)
			Expect(paths).To(BeEmpty())
			Expect(urls).To(BeEmpty())
		})

		It("reports a URL written as itself, by line", func() {
			paths, urls := found(
				"Go straight to <https://example.com/advisories/new>.",
				"The docs are at [https://example.com/docs](https://example.com/docs) and [`https://example.com`](https://example.com).",
				"Read https://example.com/guide. Then (see http://example.com/more) carry on.",
			)
			Expect(paths).To(BeEmpty())
			Expect(urls).To(Equal([]string{
				"1: <https://example.com/advisories/new>",
				"2: [https://example.com/docs](https://example.com/docs)",
				"2: [`https://example.com`](https://example.com)",
				"3: https://example.com/guide",
				"3: http://example.com/more",
			}))
		})

		It("passes a URL that is a link's destination, or a value in code", func() {
			paths, urls := found(
				"[Report it privately](https://example.com/advisories/new), or read [the guide][guide].",
				"Its audience is `https://api.example.com`, and `SKIFT_ADDRESS=http://skift.example.ts.net` reaches it.",
				"<a href=\"https://example.com\">html</a> and ![a screenshot](https://example.com/shot.png)",
				"",
				"[guide]: https://example.com/guide",
				"",
				"```sh",
				"curl -fsSLO https://example.com/docker-compose.yaml",
				"```",
			)
			Expect(paths).To(BeEmpty())
			Expect(urls).To(BeEmpty())
		})
	})
})
