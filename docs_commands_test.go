package skift_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// installGuide is the page the scenario names. Every other page is held
// to the same check.
const installGuide = "docs/install.md"

// notShipped are commands the documentation describes ahead of the
// binary, each marked as such on the page that describes it. A command
// leaves this list when it ships.
var notShipped = []string{"patch import", "node export"}

// shellWord is one word of a command line, and whether it was written in
// quotes, where a leading dash is text and not a flag.
type shellWord struct {
	text   string
	quoted bool
}

// docCommand is one skift command a page shows: where, and the words
// after "skift".
type docCommand struct {
	file  string
	line  int
	words []shellWord
}

func (c docCommand) String() string {
	texts := []string{"skift"}
	for _, w := range c.words {
		texts = append(texts, w.text)
	}
	return fmt.Sprintf("%s:%d: %s", c.file, c.line, strings.Join(texts, " "))
}

var (
	shellFence = regexp.MustCompile("^ {0,3}(```+|~~~+) *([A-Za-z0-9_+-]*)")
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// A flag as cobra prints it: an optional shorthand, the name, the
	// type of its value when it takes one, then its usage.
	helpFlag = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}|$)`)
	helpName = regexp.MustCompile(`^  ([a-z][a-z0-9-]*)\s`)
)

// shellWords splits one command line into simple commands, each a list of
// words. It knows quotes, a command substitution, a comment, and the
// operators that end a simple command; a redirection ends the words of
// the command it belongs to. That is as much shell as a page of
// documentation uses.
func shellWords(line string) [][]shellWord {
	var commands [][]shellWord
	var words []shellWord
	var word strings.Builder
	inWord, quoted, redirected := false, false, false
	endWord := func() {
		if inWord && !redirected {
			words = append(words, shellWord{text: word.String(), quoted: quoted})
		}
		word.Reset()
		inWord, quoted = false, false
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			commands = append(commands, words)
		}
		words, redirected = nil, false
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; {
		case r == '\'' || r == '"':
			if !inWord {
				inWord, quoted = true, true
			}
			for i++; i < len(runes) && runes[i] != r; i++ {
				if r == '"' && runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				word.WriteRune(runes[i])
			}
		case r == '\\' && i+1 < len(runes):
			i++
			inWord = true
			word.WriteRune(runes[i])
		case r == '$' && i+1 < len(runes) && runes[i+1] == '(':
			// A command substitution is one word, and never a flag.
			if !inWord {
				inWord, quoted = true, true
			}
			for depth := 0; i < len(runes); i++ {
				word.WriteRune(runes[i])
				if runes[i] == '(' {
					depth++
				} else if runes[i] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
		case r == ' ' || r == '\t' || r == '\\':
			endWord()
		case r == '#' && !inWord:
			endCommand()
			return commands
		case r == ';' || r == '|' || r == '&':
			endCommand()
		case r == '>' && !inWord:
			endWord()
			redirected = true
		default:
			inWord = true
			word.WriteRune(r)
		}
	}
	endCommand()
	return commands
}

// skiftCommands finds the skift commands in one Markdown file: in its
// fenced sh, bash and console blocks, every simple command whose command
// word is skift, whatever variables are set in front of it. A console
// block's commands are the lines behind a "$ " prompt. A line that begins
// with skift is read on its own even when it continues the line above, as
// the command a container runs does, and takes the lines that continue it.
func skiftCommands(file, source string) []docCommand {
	var found []docCommand
	lines := strings.Split(source, "\n")
	fence, language := "", ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if m := shellFence.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				fence, language = m[1], m[2]
			case strings.HasPrefix(m[1], fence) && m[2] == "":
				fence = ""
			}
			continue
		}
		if fence == "" || !slices.Contains([]string{"sh", "bash", "console"}, language) {
			continue
		}
		text := strings.TrimSpace(line)
		if language == "console" {
			prompted, isCommand := strings.CutPrefix(text, "$ ")
			if !isCommand {
				continue
			}
			text = prompted
		}
		at := i + 1
		if strings.HasPrefix(text, "skift ") {
			for strings.HasSuffix(text, `\`) && i+1 < len(lines) && !shellFence.MatchString(lines[i+1]) {
				i++
				text = strings.TrimSuffix(text, `\`) + " " + strings.TrimSpace(lines[i])
			}
		}
		for _, words := range shellWords(text) {
			for len(words) > 0 && !words[0].quoted && assignment.MatchString(words[0].text) {
				words = words[1:]
			}
			if len(words) > 0 && words[0].text == "skift" {
				found = append(found, docCommand{file: file, line: at, words: words[1:]})
			}
		}
	}
	return found
}

// commandHelp is what one command's --help says it offers: subcommands by
// name, and flags by name as written, each with whether a value follows
// it as a word of its own.
type commandHelp struct {
	subcommands map[string]bool
	flags       map[string]bool
}

// parseHelp reads the help cobra prints for a command.
func parseHelp(text string) commandHelp {
	help := commandHelp{subcommands: map[string]bool{}, flags: map[string]bool{}}
	section := ""
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			section = line
			continue
		}
		switch section {
		case "Available Commands:":
			if m := helpName.FindStringSubmatch(line); m != nil {
				help.subcommands[m[1]] = true
			}
		case "Flags:", "Global Flags:":
			if m := helpFlag.FindStringSubmatch(line); m != nil {
				// A value written as type[=default] is optional and never
				// a word of its own.
				takesValue := m[3] != "" && !strings.Contains(m[3], "[=")
				help.flags["--"+m[2]] = takesValue
				if m[1] != "" {
					help.flags["-"+m[1]] = takesValue
				}
			}
		}
	}
	return help
}

// missingFrom says what a documented command names that the binary does
// not have: a subcommand, looked up in the help of the command before it,
// or a flag, looked up in the help of the command the line runs. The
// first return is the command the line runs, by its words. Arguments are
// not checked.
func missingFrom(c docCommand, helpOf func(path ...string) commandHelp) (path []string, missing []string) {
	help := helpOf()
	var flags []string
	skipValue, onlyArguments := false, false
	for _, w := range c.words {
		switch {
		case skipValue:
			skipValue = false
		case onlyArguments || w.quoted || w.text == "-":
		case w.text == "--":
			onlyArguments = true
		case strings.HasPrefix(w.text, "-"):
			name, _, hasValue := strings.Cut(w.text, "=")
			if !strings.HasPrefix(name, "--") && len(name) > 2 {
				name, hasValue = name[:2], true
			}
			flags = append(flags, name)
			skipValue = help.flags[name] && !hasValue
		case len(help.subcommands) > 0:
			if !help.subcommands[w.text] {
				return append(path, w.text), []string{"no such command"}
			}
			path = append(path, w.text)
			help = helpOf(path...)
		}
	}
	for _, name := range flags {
		if _, known := help.flags[name]; !known {
			missing = append(missing, "no flag "+name)
		}
	}
	return path, missing
}

var _ = Describe("Documentation commands", Label("unit"), Serial, Ordered, func() {
	var skift string
	helps := map[string]commandHelp{}

	helpOf := func(path ...string) commandHelp {
		GinkgoHelper()
		key := strings.Join(path, " ")
		if _, cached := helps[key]; !cached {
			out, err := exec.Command(skift, append(slices.Clone(path), "--help")...).Output()
			Expect(err).NotTo(HaveOccurred(), "skift %s --help: %s", key, out)
			helps[key] = parseHelp(string(out))
		}
		return helps[key]
	}

	BeforeAll(func() {
		skift = filepath.Join(GinkgoT().TempDir(), "skift")
		build := exec.Command("go", "build", "-o", skift, "./cmd/skift")
		// A hook's git variables name a repository that may not be this one.
		for _, kv := range os.Environ() {
			if inherited(kv) {
				build.Env = append(build.Env, kv)
			}
		}
		out, err := build.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "go build: %s", out)
	})

	// documented reads the skift commands every current page shows.
	documented := func() []docCommand {
		GinkgoHelper()
		var commands []docCommand
		for _, page := range markdown(current(trackedFiles())) {
			raw, err := os.ReadFile(page)
			Expect(err).NotTo(HaveOccurred())
			commands = append(commands, skiftCommands(page, string(raw))...)
		}
		return commands
	}

	// expectFound looks each command's subcommand and flags up in the
	// binary's help, and fails naming every one the binary lacks by its
	// file, its line and what is missing. A command in notShipped is not
	// looked up.
	expectFound := func(commands []docCommand) {
		GinkgoHelper()
		var missing []string
		for _, c := range commands {
			path, problems := missingFrom(c, helpOf)
			if slices.Contains(notShipped, strings.Join(path, " ")) {
				continue
			}
			for _, problem := range problems {
				missing = append(missing, fmt.Sprintf("%s: %s", c, problem))
			}
		}
		Expect(missing).To(BeEmpty(), "documented commands the binary does not have")
	}

	It("The guide's skift commands exist", func() {
		By("given the skift binary built from this tree")
		Expect(helpOf().subcommands).To(HaveKey("ready"))
		Expect(helpOf().flags).To(HaveKeyWithValue("--project", true))

		By("and every line beginning with skift in the sh code blocks of the guide, and of every other page")
		commands := documented()
		var pages []string
		for _, c := range commands {
			pages = append(pages, c.file)
		}
		Expect(pages).To(ContainElement(installGuide), "the guide shows skift commands")

		By("when each command's subcommand and flags are looked up in its --help")
		By("then all of them are found")
		expectFound(commands)
	})

	It("Every documented skift command exists", func() {
		By("given the skift binary built from this tree")
		Expect(helpOf().subcommands).To(HaveKey("ready"))

		By("and every line beginning with skift in sh and bash code blocks of tracked Markdown outside archive")
		commands := documented()
		Expect(commands).NotTo(BeEmpty())
		for _, c := range commands {
			Expect(c.file).NotTo(HavePrefix(archiveDir))
		}

		By("when each command's subcommand and flags are looked up in its --help")
		By("then all of them are found")
		expectFound(commands)
	})

	It("A documented flag that does not exist is caught", func() {
		By("given a Markdown file whose sh block runs skift ready --no-such-flag")
		const page = "docs/example.md"
		commands := skiftCommands(page, "# Example\n\n```sh\nskift ready --no-such-flag\n```\n")
		Expect(commands).To(HaveLen(1))

		By("when the command spec runs over it")
		failures := InterceptGomegaFailures(func() { expectFound(commands) })

		By("then it fails naming the file and the flag")
		Expect(failures).To(HaveLen(1))
		Expect(failures[0]).To(ContainSubstring(page + ":4:"))
		Expect(failures[0]).To(ContainSubstring("no flag --no-such-flag"))
	})

	It("counts as not shipped only what the binary lacks", func() {
		for _, name := range notShipped {
			path := strings.Fields(name)
			parent, last := path[:len(path)-1], path[len(path)-1]
			Expect(helpOf(parent...).subcommands).NotTo(HaveKey(last),
				"skift %s has shipped: take it out of notShipped, and the note beside it out of the page that describes it", name)
		}
	})
})

var _ = Describe("Reading commands out of documentation", Label("unit"), func() {
	// words is a command as the words after skift, quoted ones in quotes.
	words := func(c docCommand) string {
		var texts []string
		for _, w := range c.words {
			if w.quoted {
				texts = append(texts, "'"+w.text+"'")
			} else {
				texts = append(texts, w.text)
			}
		}
		return strings.Join(texts, " ")
	}

	It("finds skift commands in sh, bash and console blocks, and nowhere else", func() {
		page := strings.Join([]string{
			"Run `skift prose` first.",
			"```sh",
			"skift ready --kind bug   # a comment",
			"DATABASE_URL=postgres://db/skift skift migrate",
			"skift version && skift project list",
			"sudo usermod -aG skift \"$USER\"",
			"docker compose run --rm skift \\",
			"  skift admin bootstrap 65532 <organization> \\",
			"  --tailscale-node <stable id>",
			"skift search 'socket -legacy' > found.json",
			"skift admin bootstrap $(id -u) homestead",
			"```",
			"```console",
			"$ skift claim --wait 30m",
			"skift output, not a command",
			"```",
			"```text",
			"skift text",
			"```",
			"```go",
			"skift code",
			"```",
		}, "\n")

		var found []string
		for _, c := range skiftCommands("page.md", page) {
			found = append(found, fmt.Sprintf("%d: %s", c.line, words(c)))
		}
		Expect(found).To(Equal([]string{
			"3: ready --kind bug",
			"4: migrate",
			"5: version",
			"5: project list",
			"8: admin bootstrap 65532 <organization> --tailscale-node <stable id>",
			"10: search 'socket -legacy'",
			"11: admin bootstrap '$(id -u)' homestead",
			"14: claim --wait 30m",
		}))
	})

	Describe("looking a command up", func() {
		helpOf := func(path ...string) commandHelp {
			return parseHelp(map[string]string{
				"": strings.Join([]string{
					"Available Commands:",
					"  node        Inspect nodes",
					"  ready       List the ready set",
					"",
					"Flags:",
					"  -o, --output string         output format",
					"  -p, --project stringArray   project a bare reference resolves in",
					"  -v, --version               version for skift",
				}, "\n"),
				"node": strings.Join([]string{
					"Available Commands:",
					"  show        Show a node",
					"",
					"Global Flags:",
					"  -p, --project stringArray   project a bare reference resolves in",
				}, "\n"),
				"node show": strings.Join([]string{
					"Flags:",
					"      --comments              include comments",
					"      --wait duration[=0s]    block until it changes",
					"",
					"Global Flags:",
					"  -o, --output string         output format",
					"  -p, --project stringArray   project a bare reference resolves in",
				}, "\n"),
				"ready": "Flags:\n      --kind string   only this kind\n",
			}[strings.Join(path, " ")])
		}

		DescribeTable("says what the binary does not have",
			func(line string, missing ...string) {
				commands := skiftCommands("page.md", "```sh\n"+line+"\n```\n")
				Expect(commands).To(HaveLen(1))
				_, found := missingFrom(commands[0], helpOf)
				Expect(found).To(ConsistOf(missing))
			},
			Entry("nothing, for a command and its flags", "skift ready --kind bug"),
			Entry("nothing, for a nested command with an inherited flag", "skift node show '#7' -o text --comments"),
			Entry("nothing, for a flag ahead of the command, whose value is no command", "skift -p node node show '#7'"),
			Entry("nothing, for a value joined to its flag", "skift node show '#7' --output=text -otext"),
			Entry("nothing, for a flag whose value is optional", "skift node show --wait '#7'"),
			Entry("nothing, for dashes that are arguments", "skift node show - '-7' -- --not-a-flag"),
			Entry("nothing, for flags alone", "skift --version"),
			Entry("a command that is not there", "skift claim --wait", "no such command"),
			Entry("a nested command that is not there", "skift node export '#7' --children", "no such command"),
			Entry("a flag the command lacks, though another has it", "skift ready --comments", "no flag --comments"),
			Entry("a shorthand nothing has", "skift ready -x", "no flag -x"),
		)

		It("names the command the line runs", func() {
			commands := skiftCommands("page.md", "```sh\nskift -p platform node export '#7'\n```\n")
			path, _ := missingFrom(commands[0], helpOf)
			Expect(path).To(Equal([]string{"node", "export"}))
		})
	})
})
