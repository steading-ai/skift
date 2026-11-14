package skift_test

import (
	"fmt"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"unicode"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// archiveDir holds retired material. Its files are records: nothing in
// them is checked, and they may be linked to like any other file.
const archiveDir = "archive/"

// trackedFiles lists the files git tracks in the working tree, by
// slash-separated path from the repository root.
func trackedFiles() []string {
	GinkgoHelper()
	cmd := exec.Command("git", "ls-files", "-z")
	// A hook's git variables name an index that may not be the tree's own.
	for _, kv := range os.Environ() {
		if inherited(kv) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	listed, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())
	var files []string
	for name := range strings.SplitSeq(string(listed), "\x00") {
		// A file removed from the tree and not yet from the index is gone.
		if _, err := os.Lstat(name); name != "" && err == nil {
			files = append(files, name)
		}
	}
	return files
}

// current keeps the files outside the archive.
func current(files []string) []string {
	var kept []string
	for _, name := range files {
		if !strings.HasPrefix(name, archiveDir) {
			kept = append(kept, name)
		}
	}
	return kept
}

// markdown keeps the Markdown files.
func markdown(files []string) []string {
	var kept []string
	for _, name := range files {
		if strings.HasSuffix(name, ".md") {
			kept = append(kept, name)
		}
	}
	return kept
}

// docLink is one link destination as written, and the line it is on.
type docLink struct {
	line int
	dest string
}

var (
	fenceLine  = regexp.MustCompile("^ {0,3}(```|~~~)")
	atxHeading = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	inlineLink = regexp.MustCompile(`\]\([ \t]*(<[^>]*>|[^\s()]*(?:\([^\s()]*\)[^\s()]*)*)`)
	definition = regexp.MustCompile(`^ {0,3}\[[^\]^][^\]]*\]:[ \t]*(<[^>]*>|\S+)`)
	htmlTarget = regexp.MustCompile(`\b(?:href|src)\s*=\s*["']([^"']*)["']`)
	htmlAnchor = regexp.MustCompile(`\b(?:id|name)\s*=\s*["']([^"']+)["']`)
	hasScheme  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	linkedText = regexp.MustCompile(`!?\[([^\]]*)\](?:\([^)]*\)|\[[^\]]*\])`)
	htmlTag    = regexp.MustCompile(`<[^>]+>`)
	emphasised = regexp.MustCompile(`(^|[^\pL\pN_])_+([^_]+)_+($|[^\pL\pN_])`)
	lineOfCode = regexp.MustCompile(`^L\d+(?:-L\d+)?$`)
)

// frontMatter is the line that opens and closes a file's YAML preamble.
const frontMatter = "---"

// withoutCodeSpans blanks out inline code, where brackets and angle
// brackets are literal text and not links.
func withoutCodeSpans(line string) string {
	var out strings.Builder
	for len(line) > 0 {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			break
		}
		ticks := len(line[start:]) - len(strings.TrimLeft(line[start:], "`"))
		fence := line[start : start+ticks]
		end := strings.Index(line[start+ticks:], fence)
		if end < 0 {
			break
		}
		out.WriteString(line[:start])
		out.WriteByte(' ')
		line = line[start+ticks+end+ticks:]
	}
	out.WriteString(line)
	return out.String()
}

// headingText is what a heading says once rendered: link text without its
// destination, no tags, entities decoded, and no emphasis underscores.
// Code spans keep their underscores, which are part of a name.
func headingText(raw string) string {
	raw = linkedText.ReplaceAllString(raw, "$1")
	var out strings.Builder
	for i, part := range strings.Split(raw, "`") {
		if i%2 == 0 {
			part = htmlTag.ReplaceAllString(part, "")
			part = emphasised.ReplaceAllString(part, "$1$2$3")
		}
		out.WriteString(part)
	}
	return html.UnescapeString(out.String())
}

// anchorOf turns a heading's text into the anchor GitHub and
// skift.stead.ing give it. Both follow
// https://github.com/Flet/github-slugger: lower case, every character but
// letters, digits, marks, hyphens, underscores and spaces dropped, and
// each space made a hyphen.
func anchorOf(text string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			out.WriteByte('-')
		case r == '-', r == '_', unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
			out.WriteRune(r)
		}
	}
	return out.String()
}

// proseLine is one line of a Markdown file that is neither front matter
// nor fenced code, and the number it has in the file.
type proseLine struct {
	number int
	text   string
}

// proseLines reads one Markdown file's source down to the lines a reader
// sees as prose: front matter, fenced code and the fences themselves are
// left out.
func proseLines(source string) []proseLine {
	var prose []proseLine
	lines := strings.Split(source, "\n")
	inFrontMatter := len(lines) > 0 && strings.TrimRight(lines[0], " \t\r") == frontMatter
	fence := ""
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if inFrontMatter {
			inFrontMatter = i == 0 || strings.TrimRight(line, " \t") != frontMatter
			continue
		}
		if m := fenceLine.FindStringSubmatch(line); m != nil {
			switch fence {
			case "":
				fence = m[1]
			case m[1]:
				fence = ""
			}
			continue
		}
		if fence == "" {
			prose = append(prose, proseLine{number: i + 1, text: line})
		}
	}
	return prose
}

// scanMarkdown reads one Markdown file's source: the links it makes, and
// the anchors its headings and HTML ids offer. A heading repeated in one
// file takes -1, -2 and so on, in order. Fenced code and front matter are
// skipped, and only ATX headings count.
func scanMarkdown(source string) (links []docLink, anchors map[string]bool) {
	anchors = map[string]bool{}
	seen := map[string]int{}
	offer := func(anchor string) {
		unique := anchor
		for anchors[unique] {
			seen[anchor]++
			unique = fmt.Sprintf("%s-%d", anchor, seen[anchor])
		}
		anchors[unique] = true
	}

	for _, line := range proseLines(source) {
		if m := atxHeading.FindStringSubmatch(line.text); m != nil {
			offer(anchorOf(headingText(m[1])))
		}
		prose := withoutCodeSpans(line.text)
		for _, m := range htmlAnchor.FindAllStringSubmatch(prose, -1) {
			anchors[m[1]] = true
		}
		for _, re := range []*regexp.Regexp{inlineLink, definition, htmlTarget} {
			for _, m := range re.FindAllStringSubmatch(prose, -1) {
				dest := strings.TrimSuffix(strings.TrimPrefix(m[1], "<"), ">")
				links = append(links, docLink{line: line.number, dest: dest})
			}
		}
	}
	return links, anchors
}

// linkReport is what following every link in a set of Markdown files
// found: how many were followed, which named a file that is not there,
// and which named a heading its file does not have.
type linkReport struct {
	followed        int
	missingFiles    []string
	missingHeadings []string
}

// followLinks follows every relative link and anchor in the given
// Markdown files, against the files the repository holds. A link to
// another site is not followed. A link is resolved from the file that
// makes it, as GitHub and skift.stead.ing both do; one that starts with a
// slash means a different place on each, so it is reported as missing. A
// fragment is checked when the file it lands in is Markdown, and may be a
// line reference otherwise.
func followLinks(pages, files []string, read func(string) string) linkReport {
	exists := map[string]bool{".": true}
	for _, name := range files {
		exists[name] = true
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			exists[dir] = true
		}
	}
	anchorsIn := map[string]map[string]bool{}
	linksIn := map[string][]docLink{}
	scan := func(name string) {
		if _, done := anchorsIn[name]; !done {
			linksIn[name], anchorsIn[name] = scanMarkdown(read(name))
		}
	}

	var report linkReport
	for _, page := range pages {
		scan(page)
		for _, link := range linksIn[page] {
			if link.dest == "" || hasScheme.MatchString(link.dest) || strings.HasPrefix(link.dest, "//") {
				continue
			}
			report.followed++
			where := fmt.Sprintf("%s:%d: %s", page, link.line, link.dest)

			target, fragment, _ := strings.Cut(link.dest, "#")
			target, _, _ = strings.Cut(target, "?")
			if unescaped, err := url.PathUnescape(target); err == nil {
				target = unescaped
			}
			if unescaped, err := url.PathUnescape(fragment); err == nil {
				fragment = unescaped
			}

			file := page
			if target != "" {
				file = path.Join(path.Dir(page), target)
				if strings.HasPrefix(target, "/") || !exists[file] {
					report.missingFiles = append(report.missingFiles, where)
					continue
				}
			}
			if fragment == "" {
				continue
			}
			if !strings.HasSuffix(file, ".md") {
				if !lineOfCode.MatchString(fragment) && !strings.HasPrefix(fragment, ":~:") {
					report.missingHeadings = append(report.missingHeadings, where)
				}
				continue
			}
			scan(file)
			if !anchorsIn[file][fragment] {
				report.missingHeadings = append(report.missingHeadings, where)
			}
		}
	}
	return report
}

var _ = Describe("Documentation links", Label("unit"), func() {
	readFile := func(name string) string {
		GinkgoHelper()
		raw, err := os.ReadFile(name)
		Expect(err).NotTo(HaveOccurred())
		return string(raw)
	}

	It("No documentation link is broken", func() {
		By("given every tracked Markdown file outside archive")
		files := trackedFiles()
		pages := markdown(current(files))
		Expect(pages).To(ContainElements("README.md", "AGENTS.md"))
		Expect(pages).To(ContainElement(HavePrefix("docs/")))
		Expect(pages).To(ContainElement(HavePrefix("plugin/")))
		Expect(pages).NotTo(ContainElement(HavePrefix(archiveDir)))

		By("when each relative link and anchor is followed")
		report := followLinks(pages, files, readFile)
		Expect(report.followed).To(BeNumerically(">", 0))

		By("then the file exists")
		Expect(report.missingFiles).To(BeEmpty(), "links to files that are not in the repository")

		By("and the heading exists when the link names one")
		Expect(report.missingHeadings).To(BeEmpty(), "links to headings their files do not have")
	})

	Describe("following links", func() {
		// follow runs the check over files held in memory. Every Markdown
		// file among them is a page.
		follow := func(sources map[string]string) linkReport {
			var files []string
			for name := range sources {
				files = append(files, name)
			}
			return followLinks(markdown(files), files, func(name string) string { return sources[name] })
		}

		It("passes links to files, folders and headings that exist", func() {
			report := follow(map[string]string{
				"README.md": strings.Join([]string{
					"[a page](docs/guide.md), [a folder](docs/), [a heading](docs/guide.md#set-it-up),",
					"[here](#overview), [source](cmd/main.go#L3-L9), [encoded](docs/a%20b.md),",
					"[elsewhere](https://example.com/missing.md#nowhere), <a href=\"docs/guide.md#pinned\">html</a>",
					"",
					"[defined]: docs/guide.md#set-it-up-1",
					"",
					"# Overview",
				}, "\n"),
				"docs/guide.md": "# Guide\n\n## Set it up\n\n[up](../README.md#overview)\n\n## Set it up\n\n<a id=\"pinned\"></a>\n",
				"docs/a b.md":   "# Spaced\n",
				"cmd/main.go":   "package main\n",
			})
			Expect(report.missingFiles).To(BeEmpty())
			Expect(report.missingHeadings).To(BeEmpty())
			Expect(report.followed).To(Equal(9))
		})

		It("reports a link to a file that is not there, by file and line", func() {
			report := follow(map[string]string{
				"docs/guide.md": "# Guide\n\n[gone](plans/sprint.md)\n[above the root](../../guide.md)\n[from the root](/docs/guide.md)\n",
			})
			Expect(report.missingFiles).To(Equal([]string{
				"docs/guide.md:3: plans/sprint.md",
				"docs/guide.md:4: ../../guide.md",
				"docs/guide.md:5: /docs/guide.md",
			}))
			Expect(report.missingHeadings).To(BeEmpty())
		})

		It("reports a link to a heading its file does not have", func() {
			report := follow(map[string]string{
				"docs/guide.md": "# Guide\n\n[here](#set-it-up)\n[there](other.md#guide)\n[one only](other.md#other-1)\n",
				"docs/other.md": "# Other\n",
			})
			Expect(report.missingFiles).To(BeEmpty())
			Expect(report.missingHeadings).To(Equal([]string{
				"docs/guide.md:3: #set-it-up",
				"docs/guide.md:4: other.md#guide",
				"docs/guide.md:5: other.md#other-1",
			}))
		})

		It("follows links into the archive, and none out of it", func() {
			sources := map[string]string{
				"docs/adr.md":       "# Decision\n\n[evidence](../archive/note.md#findings)\n",
				"archive/note.md":   "# Note\n\n## Findings\n\n[moved](../docs/plans/gone.md)\n",
				"archive/README.md": "[broken](nowhere.md)\n",
			}
			var files []string
			for name := range sources {
				files = append(files, name)
			}
			report := followLinks(markdown(current(files)), files, func(name string) string { return sources[name] })
			Expect(report.followed).To(Equal(1))
			Expect(report.missingFiles).To(BeEmpty())
			Expect(report.missingHeadings).To(BeEmpty())
		})

		It("reads no link or heading out of code", func() {
			links, anchors := scanMarkdown(strings.Join([]string{
				"---",
				"title: [not a link](front-matter.md)",
				"---",
				"# Real",
				"```markdown",
				"# Fenced",
				"[fenced](fenced.md)",
				"```",
				"Inline `[code](code.md)` and ``[more ` code](more.md)``, then [real](real.md).",
				"[^1]: a footnote, not a definition",
			}, "\n"))
			Expect(links).To(Equal([]docLink{{line: 9, dest: "real.md"}}))
			Expect(anchors).To(Equal(map[string]bool{"real": true}))
		})
	})

	DescribeTable("a heading's anchor",
		func(heading, anchor string) {
			_, anchors := scanMarkdown(heading)
			Expect(anchors).To(Equal(map[string]bool{anchor: true}))
		},
		Entry("is lower case, with a hyphen for each space", "## Completion and results", "completion-and-results"),
		Entry("drops punctuation, and keeps the hyphens either side of it", "## M0 — Walking skeleton", "m0--walking-skeleton"),
		Entry("keeps the words of a code span", "### A hold node with `blocks` edges", "a-hold-node-with-blocks-edges"),
		Entry("keeps an underscore inside a name", "### The `assignee_kind` field", "the-assignee_kind-field"),
		Entry("drops the underscores of emphasis", "## An _important_ rule", "an-important-rule"),
		Entry("takes a link's text and not its destination", "## See [the model](domain-model.md#keys)", "see-the-model"),
		Entry("drops tags and decodes entities", "## H<sub>2</sub>O &amp; more", "h2o--more"),
		Entry("ignores a closing run of hashes", "## Closed ##", "closed"),
		Entry("keeps letters outside ASCII", "## Diátaxis", "diátaxis"),
	)
})
