package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/skift/internal/client"
)

// skift doctor is run as a process, the way a person or an agent runs it
// after installing: its MCP check starts the binary it is, so the binary
// under test has to be the real one.
var _ = Describe("skift doctor", Label("unit"), func() {
	type check struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
		Fix    string `json:"fix"`
	}
	type report struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}

	var (
		binary string
		s      *stub
		socket string
		home   string
		work   string
		env    []string
	)

	BeforeEach(func() {
		var err error
		binary, err = gexec.Build("github.com/steading-ai/skift/cmd/skift")
		Expect(err).NotTo(HaveOccurred())
		s = newStub()
		socket = serve(s)
		home = GinkgoT().TempDir()
		work = GinkgoT().TempDir()
		env = []string{
			"HOME=" + home,
			"PATH=" + os.Getenv("PATH"),
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			client.EnvConfig + "=", client.EnvAddress + "=", client.EnvProject + "=",
			client.EnvSocket + "=" + socket,
		}
	})

	// loginShell is a stand-in for $SHELL that prints what `command -v
	// skift` would, or fails as a shell does when it finds nothing.
	loginShell := func(finds string) string {
		GinkgoHelper()
		path := filepath.Join(GinkgoT().TempDir(), "shell")
		body := "#!/bin/sh\necho 'profile noise'\nexit 1\n"
		if finds != "" {
			body = "#!/bin/sh\necho 'profile noise'\necho '" + finds + "'\n"
		}
		Expect(os.WriteFile(path, []byte(body), 0o755)).To(Succeed()) //nolint:gosec // a script the spec runs
		return path
	}

	installSkill := func() string {
		GinkgoHelper()
		dir := filepath.Join(home, ".claude", "plugins", "cache", "skift", "skift", "0.1.0", "skills", "skift")
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: skift\n---\n"), 0o600)).To(Succeed())
		return dir
	}

	run := func(extra ...string) (report, int, string) {
		GinkgoHelper()
		cmd := exec.Command(binary, append([]string{"doctor", "--output", "json"}, extra...)...)
		cmd.Env = env
		cmd.Dir = work
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			Expect(errors.As(err, &exitErr)).To(BeTrue(), err.Error())
			code = exitErr.ExitCode()
		}
		var r report
		Expect(json.Unmarshal(stdout.Bytes(), &r)).To(Succeed(), stdout.String()+stderr.String())
		return r, code, stderr.String()
	}

	find := func(r report, name string) check {
		GinkgoHelper()
		for _, c := range r.Checks {
			if c.Name == name {
				return c
			}
		}
		Fail("no check named " + name)
		return check{}
	}

	Describe("Scenario: A workstation that is set up passes every check", func() {
		It("A workstation that is set up passes every check", func() {
			By("given skift on a login shell's PATH, a service that knows the caller, and the skill installed")
			env = append(env, "SHELL="+loginShell(binary))
			skill := installSkill()

			By("when doctor runs")
			r, code, stderr := run()

			By("then every check is ok and the exit status is zero")
			Expect(code).To(Equal(0), stderr)
			Expect(r.OK).To(BeTrue())
			Expect(r.Checks).To(HaveLen(6))
			for _, c := range r.Checks {
				Expect(c.Status).To(Equal("ok"), c.Name+": "+c.Detail)
				Expect(c.Fix).To(BeEmpty())
			}
			Expect(find(r, "path").Detail).To(ContainSubstring(binary), "the shell's profile output is not mistaken for the path")
			Expect(find(r, "config").Detail).To(ContainSubstring(socket))
			Expect(find(r, "mcp").Detail).To(MatchRegexp(`listed \d+ tools`))
			Expect(find(r, "skills").Detail).To(ContainSubstring(skill))
		})
	})

	Describe("Scenario: A login shell cannot find skift", func() {
		It("A login shell cannot find skift", func() {
			By("given a login shell whose PATH lacks skift")
			env = append(env, "SHELL="+loginShell(""))
			installSkill()

			By("when doctor runs")
			r, code, stderr := run()

			By("then the path check fails, points at mise's shims, and the exit status is non-zero")
			Expect(code).To(Equal(1))
			Expect(r.OK).To(BeFalse())
			path := find(r, "path")
			Expect(path.Status).To(Equal("fail"))
			Expect(path.Detail).To(ContainSubstring("skift mcp"))
			Expect(path.Fix).To(ContainSubstring("https://mise.jdx.dev/dev-tools/shims.html"))
			Expect(stderr).To(ContainSubstring(`"code":"failed_precondition"`))
		})
	})

	It("warns when a login shell finds a different skift", func() {
		other := filepath.Join(GinkgoT().TempDir(), "skift")
		Expect(os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)).To(Succeed()) //nolint:gosec // a stand-in binary
		env = append(env, "SHELL="+loginShell(other))
		installSkill()

		r, code, _ := run()

		Expect(code).To(Equal(0), "a warning does not fail the run")
		path := find(r, "path")
		Expect(path.Status).To(Equal("warn"))
		Expect(path.Detail).To(ContainSubstring(other))
		Expect(path.Fix).NotTo(BeEmpty())
	})

	Describe("Scenario: The service does not know this caller", func() {
		It("The service does not know this caller", func() {
			By("given a service that maps the caller to no principal")
			env = append(env, "SHELL="+loginShell(binary))
			s.unauthenticated = true

			By("when doctor runs")
			r, code, _ := run()

			By("then the service check fails and names the bootstrap command")
			Expect(code).To(Equal(1))
			service := find(r, "service")
			Expect(service.Status).To(Equal("fail"))
			Expect(service.Detail).To(ContainSubstring("does not know this caller"))
			Expect(service.Fix).To(ContainSubstring("skift admin bootstrap"))
		})
	})

	Describe("Scenario: No service answers", func() {
		It("No service answers", func() {
			By("given a socket nothing listens on")
			env = append(env, "SHELL="+loginShell(binary), client.EnvSocket+"="+filepath.Join(GinkgoT().TempDir(), "none.sock"))

			By("when doctor runs")
			r, code, _ := run()

			By("then the service check fails with what to do")
			Expect(code).To(Equal(1))
			service := find(r, "service")
			Expect(service.Status).To(Equal("fail"))
			Expect(service.Detail).To(ContainSubstring("cannot reach"))
			Expect(service.Fix).To(ContainSubstring("config.json"))
		})
	})

	Describe("Scenario: The address is only in the shell's environment", func() {
		It("The address is only in the shell's environment", func() {
			By("given $SKIFT_ADDRESS and no config file")
			env = append(env, "SHELL="+loginShell(binary), client.EnvAddress+"=http://127.0.0.1:1")

			By("when doctor runs")
			r, _, _ := run()

			By("then the config check warns and says to put the address in the config file")
			config := find(r, "config")
			Expect(config.Status).To(Equal("warn"))
			Expect(config.Detail).To(ContainSubstring("$SKIFT_ADDRESS"))
			Expect(config.Fix).To(ContainSubstring(filepath.Join(home, ".config", "skift", "config.json")))
			Expect(config.Fix).To(ContainSubstring(`"address"`))
		})
	})

	It("passes the config check when the address is in the config file", func() {
		dir := filepath.Join(home, ".config", "skift")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"address": "http://127.0.0.1:1"}`), 0o600)).To(Succeed())
		env = append(env, "SHELL="+loginShell(binary))

		r, _, _ := run()

		config := find(r, "config")
		Expect(config.Status).To(Equal("ok"))
		Expect(config.Detail).To(ContainSubstring("from the config file"))
	})

	It("fails the config check on a config file that does not parse, and does not try the service", func() {
		dir := filepath.Join(home, ".config", "skift")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"address": `), 0o600)).To(Succeed())
		env = append(env, "SHELL="+loginShell(binary))

		r, code, _ := run()

		Expect(code).To(Equal(1))
		Expect(find(r, "config").Status).To(Equal("fail"))
		Expect(find(r, "service").Detail).To(ContainSubstring("not checked"))
	})

	It("warns, without failing, when no skill is installed", func() {
		env = append(env, "SHELL="+loginShell(binary))

		r, code, _ := run()

		Expect(code).To(Equal(0))
		skills := find(r, "skills")
		Expect(skills.Status).To(Equal("warn"))
		Expect(skills.Fix).To(ContainSubstring("https://skift.stead.ing/docs/agent-skills/"))
	})

	It("prints one line per check, with its fix beneath, as text", func() {
		env = append(env, "SHELL="+loginShell(""))
		cmd := exec.Command(binary, "doctor", "--output", "text")
		cmd.Env = env
		cmd.Dir = work
		out, err := cmd.Output()
		Expect(err).To(HaveOccurred(), "a failed check fails the run")
		Expect(string(out)).To(MatchRegexp(`(?m)^fail\s+path\s+a login shell`))
		Expect(string(out)).To(MatchRegexp(`(?m)^\s+fix: .*shims`))
		Expect(string(out)).To(MatchRegexp(`(?m)^ok\s+binary\s+skift `))
	})
})
