package skift_test

import (
	"os"
	"path/filepath"
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
	Jobs        map[string]workflowJob     `yaml:"jobs"`
}

type workflowTrigger struct {
	Inputs map[string]any `yaml:"inputs"`
	Tags   []string       `yaml:"tags"`
}

type workflowJob struct {
	Needs       stringOrList      `yaml:"needs"`
	Uses        string            `yaml:"uses"`
	Environment string            `yaml:"environment"`
	Permissions map[string]string `yaml:"permissions"`
	Strategy    struct {
		Matrix struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
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
	return w
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

// commands splits a run script into its commands, one per line once
// backslash continuations are joined.
func commands(script string) []string {
	return strings.Split(strings.ReplaceAll(script, "\\\n", " "), "\n")
}
