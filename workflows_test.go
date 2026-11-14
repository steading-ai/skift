package skift_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// workflow is the part of a GitHub Actions workflow the specs reason
// about: what starts it, what each job may do, and what each step runs.
type workflow struct {
	On          map[string]workflowTrigger `yaml:"on"`
	Permissions map[string]string          `yaml:"permissions"`
	Env         map[string]string          `yaml:"env"`
	Jobs        map[string]workflowJob     `yaml:"jobs"`

	// source is the file as written, for what the fields above leave out.
	source string
}

type workflowTrigger struct {
	Inputs  map[string]any `yaml:"inputs"`
	Outputs map[string]any `yaml:"outputs"`
	Tags    []string       `yaml:"tags"`
}

type workflowJob struct {
	RunsOn      any               `yaml:"runs-on"`
	Needs       stringOrList      `yaml:"needs"`
	Uses        string            `yaml:"uses"`
	Environment string            `yaml:"environment"`
	Permissions map[string]string `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Strategy    struct {
		Matrix struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

// stringOrList reads a field GitHub lets be one name or a list of them.
type stringOrList []string

func (s *stringOrList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*s = []string{node.Value}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

// loadWorkflow parses a workflow under .github/workflows.
func loadWorkflow(name string) workflow {
	GinkgoHelper()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", name))
	Expect(err).NotTo(HaveOccurred())
	var w workflow
	Expect(yaml.Unmarshal(raw, &w)).To(Succeed(), name)
	w.source = string(raw)
	return w
}

// secretsContext matches each use of the secrets context, capturing the
// secret's name where the use names one.
var secretsContext = regexp.MustCompile(`\bsecrets\b(?:\.(\w+))?`)

// secrets names the secret read at each place the workflow uses the
// secrets context, anywhere in the file. A use that names no single
// secret, such as passing every secret on, is an empty name.
func (w workflow) secrets() []string {
	var names []string
	for _, use := range secretsContext.FindAllStringSubmatch(w.source, -1) {
		names = append(names, use[1])
	}
	return names
}

// triggers names the events that start a workflow.
func (w workflow) triggers() []string {
	names := make([]string, 0, len(w.On))
	for name := range w.On {
		names = append(names, name)
	}
	return names
}

// scripts returns every step's run script, in no particular order.
func (w workflow) scripts() []string {
	var scripts []string
	for _, job := range w.Jobs {
		for _, step := range job.Steps {
			if step.Run != "" {
				scripts = append(scripts, step.Run)
			}
		}
	}
	return scripts
}

// steps returns every step of every job using the named action, at any
// version, in no particular order across jobs.
func (w workflow) steps(action string) []workflowStep {
	var steps []workflowStep
	for _, job := range w.Jobs {
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, action+"@") {
				steps = append(steps, step)
			}
		}
	}
	return steps
}

// text is everything a step says, as YAML: its action and inputs, its
// script, and the environment it sets.
func (s workflowStep) text() string {
	GinkgoHelper()
	raw, err := yaml.Marshal(s)
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}

// commands splits a run script into its commands, one per line once
// backslash continuations are joined.
func commands(script string) []string {
	return strings.Split(strings.ReplaceAll(script, "\\\n", " "), "\n")
}
