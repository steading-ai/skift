package skift_test

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// retiredPath is where a piece of retired machinery lives now, and how
// many trailing elements of that path are the name it went by.
type retiredPath struct {
	path     string
	elements int
}

// retired is spelled as archive paths so that this file names nothing
// retired itself: it is one of the files searched.
var retired = []retiredPath{
	{"archive/harness/", 1},
	{"archive/scripts/ready.py", 2},
	{"archive/backlog.toml", 1},
	{"archive/claude-skills/ralph-loop", 1},
	{"archive/AGENT_REVIEW.md", 1},
}

// split gives the directory the path sits in and the name it went by.
func (r retiredPath) split() (home, name string) {
	parts := strings.Split(strings.TrimSuffix(r.path, "/"), "/")
	home = strings.Join(parts[:len(parts)-r.elements], "/") + "/"
	return home, strings.TrimPrefix(r.path, home)
}

// strayMentions finds every place the text names retired machinery
// without pointing at its place in the archive, as file:line: text. A
// name that is the tail of a longer word is another name.
func strayMentions(file, text string) []string {
	var stray []string
	for n, line := range strings.Split(text, "\n") {
		for _, r := range retired {
			home, name := r.split()
			for from := 0; ; {
				at := strings.Index(line[from:], name)
				if at < 0 {
					break
				}
				at += from
				from = at + len(name)
				before, _ := utf8.DecodeLastRuneInString(line[:at])
				if unicode.IsLetter(before) || unicode.IsDigit(before) {
					continue
				}
				if !strings.HasSuffix(line[:at], home) {
					stray = append(stray, fmt.Sprintf("%s:%d: %s", file, n+1, strings.TrimSpace(line)))
				}
			}
		}
	}
	return stray
}

var _ = Describe("The archive", Label("unit"), func() {
	It("holds everything that was retired, beside a README", func() {
		for _, r := range retired {
			Expect(strings.TrimSuffix(r.path, "/")).To(Or(BeADirectory(), BeAnExistingFile()))
		}
		Expect("archive/README.md").To(BeAnExistingFile())
	})

	It("Nothing current names the retired machinery", func() {
		By("given the tracked files outside archive")
		files := current(trackedFiles())
		Expect(files).To(ContainElements("README.md", "AGENTS.md", "Makefile", "mise.toml", ".gitignore"))

		By("when they are searched for the retired paths")
		var stray []string
		for _, name := range files {
			raw, err := os.ReadFile(name)
			Expect(err).NotTo(HaveOccurred())
			if bytes.IndexByte(raw, 0) >= 0 {
				continue // not text
			}
			stray = append(stray, strayMentions(name, string(raw))...)
		}

		By("then every match is a path under archive")
		Expect(stray).To(BeEmpty(), "retired machinery named outside archive/")
	})

	It("tells a mention of the archive from a mention of what was retired", func() {
		in := func(text string) []string { return strayMentions("notes.md", text) }
		// bare is the name a retired path went by, as text would have it.
		bare := func(r retiredPath) string {
			_, name := r.split()
			return name
		}
		loop, ready, backlog, skill, review := bare(retired[0]), bare(retired[1]), bare(retired[2]), bare(retired[3]), bare(retired[4])

		Expect(in("See archive/backlog.toml and [the loop](../archive/harness/loop.py).")).To(BeEmpty())
		Expect(in("the skill in archive/claude-skills/ralph-loop/SKILL.md")).To(BeEmpty())
		Expect(in("specs live in internal/test"+loop)).To(BeEmpty(), "the tail of a longer word")

		Expect(in("first line\nrun " + loop + "loop.py")).To(Equal([]string{"notes.md:2: run " + loop + "loop.py"}))
		Expect(in("archive/backlog.toml replaced " + backlog)).To(HaveLen(1))
		Expect(in(ready + " --all")).To(HaveLen(1))
		Expect(in("the /" + skill + " skill")).To(HaveLen(1))
		Expect(in(review + " §3")).To(HaveLen(1))
	})
})
