package skift_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// skillsDir holds the skills the plugin ships, one directory each.
const skillsDir = "plugin/skift/skills"

// agentSkillsFields are the frontmatter fields the Agent Skills
// specification defines: https://agentskills.io/specification#frontmatter
var agentSkillsFields = []string{
	"name", "description", "license", "compatibility", "metadata", "allowed-tools",
}

// harnessToolPrefix opens the name a harness gives an MCP server's tool,
// as against the name the server gives it.
const harnessToolPrefix = "mcp__"

// skillFile splits a SKILL.md into the YAML between the two "---" lines it
// opens with, parsed, and the instructions after them.
func skillFile(path string) (fields map[string]any, body string) {
	GinkgoHelper()
	raw, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())

	rest, found := strings.CutPrefix(string(raw), "---\n")
	Expect(found).To(BeTrue(), "%s opens with a frontmatter fence", path)
	block, body, found := strings.Cut(rest, "\n---\n")
	Expect(found).To(BeTrue(), "%s closes its frontmatter", path)

	Expect(yaml.Unmarshal([]byte(block), &fields)).To(Succeed(), path)
	return fields, body
}

// instructions is what a file under skillsDir tells an agent: all of it,
// except a SKILL.md's allowed-tools, whose value is permission patterns
// for a harness rather than instruction.
func instructions(path string) string {
	GinkgoHelper()
	if filepath.Base(path) != "SKILL.md" {
		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		return string(raw)
	}
	fields, body := skillFile(path)
	delete(fields, "allowed-tools")
	rest, err := yaml.Marshal(fields)
	Expect(err).NotTo(HaveOccurred())
	return string(rest) + body
}

var _ = Describe("The agent skills", Label("unit"), func() {
	It("A skill's frontmatter is portable", func() {
		By("given each SKILL.md under plugin/skift/skills")
		paths, err := filepath.Glob(filepath.Join(skillsDir, "*", "SKILL.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(paths).NotTo(BeEmpty())

		for _, path := range paths {
			By("when the frontmatter of " + path + " is parsed")
			fields, _ := skillFile(path)

			By("then it has a name and a description")
			Expect(fields).To(HaveKeyWithValue("name", MatchRegexp(`^[a-z0-9]+(-[a-z0-9]+)*$`)), path)
			Expect(fields["name"]).To(Equal(filepath.Base(filepath.Dir(path))),
				"the specification has a skill named after its directory")
			Expect(fields).To(HaveKeyWithValue("description", BeAssignableToTypeOf("")), path)
			Expect(utf8.RuneCountInString(fields["description"].(string))).To(
				And(BeNumerically(">", 0), BeNumerically("<=", 1024)), path)

			By("and every key is one the Agent Skills specification defines")
			for key := range fields {
				Expect(agentSkillsFields).To(ContainElement(key), path)
			}
			if tools, declared := fields["allowed-tools"]; declared {
				Expect(tools).To(BeAssignableToTypeOf(""),
					"%s: the specification has allowed-tools a space-separated string", path)
			}
		}
	})

	It("The skills name no harness-specific tool", func() {
		By("given the files under plugin/skift/skills")
		var files []string
		Expect(filepath.WalkDir(skillsDir, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() {
				files = append(files, path)
			}
			return err
		})).To(Succeed())
		Expect(files).NotTo(BeEmpty())

		By("when they are searched for \"mcp__\", outside a SKILL.md's allowed-tools")
		var found []string
		for _, path := range files {
			if strings.Contains(instructions(path), harnessToolPrefix) {
				found = append(found, path)
			}
		}

		By("then nothing is found")
		Expect(found).To(BeEmpty())
	})

	It("are reached from the marketplace manifest at the repository root", func() {
		marketplace := readManifest(".claude-plugin/marketplace.json")
		Expect(marketplace.Plugins).To(HaveLen(1))

		entry := marketplace.Plugins[0]
		Expect(entry.Source).To(HavePrefix("./"), "a path inside the repository")
		Expect(filepath.Join(entry.Source, "skills")).To(Equal(skillsDir))
		Expect(readManifest(filepath.Join(entry.Source, ".claude-plugin", "plugin.json")).Name).To(Equal(entry.Name))
	})
})
