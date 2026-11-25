package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/client"
	"github.com/steading-ai/skift/internal/version"
)

// skiftBinary is skift built once per spec process, for the specs that
// need a real one in a process of its own.
var skiftBinary = func() func() string {
	var built string
	return func() string {
		GinkgoHelper()
		if built == "" {
			var err error
			built, err = gexec.Build("github.com/steading-ai/skift/cmd/skift")
			Expect(err).NotTo(HaveOccurred())
		}
		return built
	}
}()

// skift doctor runs in this process, against a stub service, with a
// stand-in for skift on PATH and for the login shell. Only its mcp check
// starts a real skift, the built binary.
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
		s      *stub
		socket string
		home   string
		config string
	)

	// script writes an executable shell script and returns its path.
	script := func(dir, name, body string) string {
		GinkgoHelper()
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755)).To(Succeed()) //nolint:gosec // a script the spec runs
		return path
	}

	// onPath puts a skift reporting ver on this process's PATH, and
	// returns it.
	onPath := func(ver string) string {
		GinkgoHelper()
		dir := GinkgoT().TempDir()
		GinkgoT().Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
		return script(dir, "skift", "echo 'skift version "+ver+"'\n")
	}

	// loginShell is a stand-in for $SHELL whose profile is noisy, and
	// which finds skift at path reporting ver, or finds none when path is
	// empty.
	loginShell := func(path, ver string) {
		GinkgoHelper()
		body := "echo 'profile noise'\n"
		if path != "" {
			body += "echo '" + path + "'\necho 'skift version " + ver + "'\n"
		} else {
			body += "echo 'skift: command not found' >&2\nexit 127\n"
		}
		GinkgoT().Setenv("SHELL", script(GinkgoT().TempDir(), "shell", body))
	}

	// found sets up skift the way a working install has it: the same
	// version on this PATH and a login shell's.
	found := func() {
		GinkgoHelper()
		loginShell(onPath(version.String()), version.String())
	}

	writeConfig := func(body string) {
		GinkgoHelper()
		Expect(os.MkdirAll(filepath.Dir(config), 0o700)).To(Succeed())
		Expect(os.WriteFile(config, []byte(body), 0o600)).To(Succeed())
	}

	installSkill := func(under ...string) string {
		GinkgoHelper()
		dir := filepath.Join(append(append([]string{home}, under...), "skift")...)
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: skift\n---\n"), 0o600)).To(Succeed())
		return dir
	}
	pluginCache := []string{".claude", "plugins", "cache", "skift", "skift", "0.5.0", "skills"}

	run := func(args ...string) (report, int, string) {
		GinkgoHelper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(append([]string{"doctor", "--output", "json"}, args...), strings.NewReader(""), &stdout, &stderr)
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

	BeforeEach(func() {
		s = newStub()
		socket = serve(s)
		home = GinkgoT().TempDir()
		config = filepath.Join(home, ".config", "skift", "config.json")
		GinkgoT().Setenv("HOME", home)
		GinkgoT().Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		for _, k := range []string{client.EnvConfig, client.EnvSocket, client.EnvAddress, client.EnvProject} {
			GinkgoT().Setenv(k, "")
		}
		writeConfig(`{"socket": "` + socket + `"}`)
		DeferCleanup(cli.SetDoctorExecutable(skiftBinary(), nil))
	})

	Describe("Scenario: A workstation that is set up passes every check", func() {
		It("A workstation that is set up passes every check", func() {
			By("given skift on this shell's and a login shell's PATH, a service that knows the caller, and the skill installed")
			found()
			skill := installSkill(pluginCache...)

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
			Expect(find(r, "binary").Detail).To(ContainSubstring(skiftBinary()))
			Expect(find(r, "path").Detail).To(ContainSubstring("skift "+version.String()), "the shell's profile output is not mistaken for the answer")
			Expect(find(r, "config").Detail).To(ContainSubstring("socket " + socket + ", from the config file"))
			Expect(find(r, "service").Detail).To(ContainSubstring("answered"))
			Expect(find(r, "mcp").Detail).To(MatchRegexp(`listed \d+ tools, and answered project_list`))
			Expect(find(r, "skills").Detail).To(ContainSubstring(skill))
		})
	})

	Describe("the path check", func() {
		BeforeEach(func() { installSkill(pluginCache...) })

		Describe("Scenario: No shell finds skift", func() {
			It("No shell finds skift", func() {
				By("given neither this shell's PATH nor a login shell's has skift")
				GinkgoT().Setenv("PATH", GinkgoT().TempDir())
				loginShell("", "")

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

		Describe("Scenario: Only an interactive shell finds skift", func() {
			It("Only an interactive shell finds skift", func() {
				By("given skift on this shell's PATH but not a login shell's")
				onPath(version.String())
				loginShell("", "")

				By("when doctor runs")
				r, code, _ := run()

				By("then the path check warns that a harness started outside a terminal cannot start skift mcp")
				Expect(code).To(Equal(0), "a warning does not fail the run")
				path := find(r, "path")
				Expect(path.Status).To(Equal("warn"))
				Expect(path.Detail).To(ContainSubstring("outside a terminal"))
				Expect(path.Fix).To(ContainSubstring("shims"))
			})
		})

		It("warns when only a login shell finds skift", func() {
			GinkgoT().Setenv("PATH", GinkgoT().TempDir())
			loginShell("/opt/skift/bin/skift", version.String())

			r, code, _ := run()

			Expect(code).To(Equal(0))
			path := find(r, "path")
			Expect(path.Status).To(Equal("warn"))
			Expect(path.Detail).To(ContainSubstring("/opt/skift/bin/skift"))
			Expect(path.Detail).To(ContainSubstring("this shell's PATH has no skift"))
		})

		Describe("Scenario: A tool manager's shim runs this version", func() {
			It("A tool manager's shim runs this version", func() {
				By("given skift on PATH as a symlink to a tool manager, which runs this version")
				manager := script(GinkgoT().TempDir(), "mise", "echo 'skift version "+version.String()+"'\n")
				shims := GinkgoT().TempDir()
				shim := filepath.Join(shims, "skift")
				Expect(os.Symlink(manager, shim)).To(Succeed())
				GinkgoT().Setenv("PATH", shims+string(os.PathListSeparator)+"/usr/bin:/bin")
				loginShell(shim, version.String())

				By("when doctor runs")
				r, _, _ := run()

				By("then the path check is ok, though the shim is not this binary")
				path := find(r, "path")
				Expect(path.Status).To(Equal("ok"), path.Detail)
				Expect(path.Detail).To(ContainSubstring(shim))
			})
		})

		Describe("Scenario: A harness would start another version", func() {
			It("A harness would start another version", func() {
				By("given a login shell that finds an older skift")
				onPath(version.String())
				loginShell("/usr/local/bin/skift", "0.0.1")

				By("when doctor runs")
				r, code, _ := run()

				By("then the path check warns, naming both versions and the other copy")
				Expect(code).To(Equal(0))
				path := find(r, "path")
				Expect(path.Status).To(Equal("warn"))
				Expect(path.Detail).To(ContainSubstring("this is skift " + version.String()))
				Expect(path.Detail).To(ContainSubstring("skift 0.0.1 at /usr/local/bin/skift"))
				Expect(path.Fix).To(ContainSubstring("remove the other copy"))
			})
		})

		It("warns about a skift that does not report its version", func() {
			onPath(version.String())
			GinkgoT().Setenv("SHELL", script(GinkgoT().TempDir(), "shell", "echo /usr/local/bin/skift\n"))

			r, _, _ := run()

			path := find(r, "path")
			Expect(path.Status).To(Equal("warn"))
			Expect(path.Detail).To(ContainSubstring("did not report its version"))
		})
	})

	Describe("the config check", func() {
		BeforeEach(func() {
			found()
			installSkill(pluginCache...)
		})

		Describe("Scenario: The address is only in the shell's environment", func() {
			It("The address is only in the shell's environment", func() {
				By("given $SKIFT_ADDRESS and no config file")
				Expect(os.Remove(config)).To(Succeed())
				GinkgoT().Setenv(client.EnvAddress, "http://127.0.0.1:1")

				By("when doctor runs")
				r, _, _ := run()

				By("then the config check warns and says to put the address in the config file")
				cfg := find(r, "config")
				Expect(cfg.Status).To(Equal("warn"))
				Expect(cfg.Detail).To(ContainSubstring("address http://127.0.0.1:1, from $SKIFT_ADDRESS"))
				Expect(cfg.Detail).To(ContainSubstring(config + " (absent)"))
				Expect(cfg.Fix).To(ContainSubstring(`{"address": "http://127.0.0.1:1"} in ` + config))
			})
		})

		It("warns about a socket that is only in the environment, the same way", func() {
			Expect(os.Remove(config)).To(Succeed())
			GinkgoT().Setenv(client.EnvSocket, socket)

			r, code, _ := run()

			Expect(code).To(Equal(0))
			cfg := find(r, "config")
			Expect(cfg.Status).To(Equal("warn"))
			Expect(cfg.Detail).To(ContainSubstring("from $SKIFT_SOCKET"))
			Expect(cfg.Fix).To(ContainSubstring(`{"socket": "` + socket + `"} in ` + config))
		})

		It("passes an address in the config file", func() {
			writeConfig(`{"address": "http://127.0.0.1:1"}`)

			r, _, _ := run()

			cfg := find(r, "config")
			Expect(cfg.Status).To(Equal("ok"))
			Expect(cfg.Detail).To(ContainSubstring("from the config file"))
		})

		It("credits a flag, and passes it on to the MCP server it checks", func() {
			Expect(os.Remove(config)).To(Succeed())

			r, code, stderr := run("--socket", socket)

			Expect(code).To(Equal(0), stderr)
			Expect(find(r, "config").Detail).To(ContainSubstring("socket " + socket + ", from --socket"))
			Expect(find(r, "mcp").Status).To(Equal("ok"), find(r, "mcp").Detail)
		})

		It("names the default socket when nothing sets one", func() {
			Expect(os.Remove(config)).To(Succeed())

			r, _, _ := run()

			Expect(find(r, "config").Detail).To(ContainSubstring("socket " + client.DefaultSocketPath() + ", from the default"))
		})

		Describe("Scenario: The config file does not parse", func() {
			It("The config file does not parse", func() {
				By("given a config file that is not JSON")
				writeConfig(`{"address": `)

				By("when doctor runs")
				r, code, _ := run()

				By("then the config check fails naming the file, the service is not tried, and the MCP server reports the same error")
				Expect(code).To(Equal(1))
				cfg := find(r, "config")
				Expect(cfg.Status).To(Equal("fail"))
				Expect(cfg.Fix).To(ContainSubstring("correct or remove " + config))
				Expect(find(r, "service").Detail).To(ContainSubstring("not checked"))
				mcp := find(r, "mcp")
				Expect(mcp.Status).To(Equal("fail"), "a server that has no service is not ok, though it lists its tools")
				Expect(mcp.Detail).To(ContainSubstring("parse config"))
			})
		})
	})

	Describe("the service check", func() {
		BeforeEach(func() {
			found()
			installSkill(pluginCache...)
		})

		Describe("Scenario: The service does not know this caller", func() {
			It("The service does not know this caller", func() {
				By("given a service that maps the caller to no principal")
				s.mu.Lock()
				s.unauthenticated = true
				s.mu.Unlock()

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
				writeConfig(`{"socket": "` + filepath.Join(GinkgoT().TempDir(), "none.sock") + `"}`)

				By("when doctor runs")
				r, code, _ := run()

				By("then the service and MCP checks fail, and the fix names the config file")
				Expect(code).To(Equal(1))
				service := find(r, "service")
				Expect(service.Status).To(Equal("fail"))
				Expect(service.Detail).To(ContainSubstring("cannot reach"))
				Expect(service.Fix).To(ContainSubstring(config))
				mcp := find(r, "mcp")
				Expect(mcp.Status).To(Equal("fail"))
				Expect(mcp.Detail).To(ContainSubstring("project_list failed"))
			})
		})

		It("reports an address that does not reach a service", func() {
			writeConfig(`{"address": "http://127.0.0.1:1"}`)

			r, _, _ := run()

			service := find(r, "service")
			Expect(service.Status).To(Equal("fail"))
			Expect(service.Fix).To(ContainSubstring("tailnet"))
		})
	})

	Describe("the mcp check", func() {
		BeforeEach(func() {
			found()
			installSkill(pluginCache...)
		})

		It("fails, without starting anything, when doctor cannot find its own binary", func() {
			DeferCleanup(cli.SetDoctorExecutable("", errors.New("no such binary")))

			r, code, _ := run()

			Expect(code).To(Equal(1))
			mcp := find(r, "mcp")
			Expect(mcp.Status).To(Equal("fail"))
			Expect(mcp.Detail).To(ContainSubstring("no such binary"))
		})

		It("fails with the server's own account when it does not start", func() {
			broken := script(GinkgoT().TempDir(), "skift", "echo 'skift: cannot start' >&2\nexit 3\n")
			DeferCleanup(cli.SetDoctorExecutable(broken, nil))

			r, code, _ := run()

			Expect(code).To(Equal(1))
			mcp := find(r, "mcp")
			Expect(mcp.Status).To(Equal("fail"))
			Expect(mcp.Detail).To(ContainSubstring("skift mcp did not start: skift: cannot start"))
			Expect(mcp.Fix).NotTo(BeEmpty())
		})
	})

	Describe("the skills check", func() {
		BeforeEach(found)

		It("warns, without failing, when no skill is installed", func() {
			r, code, _ := run()

			Expect(code).To(Equal(0))
			skills := find(r, "skills")
			Expect(skills.Status).To(Equal("warn"))
			Expect(skills.Fix).To(ContainSubstring("https://skift.stead.ing/docs/agent-skills/"))
		})

		It("finds the skill where an installer puts it outside the plugin", func() {
			skill := installSkill(".agents", "skills")

			r, _, _ := run()

			skills := find(r, "skills")
			Expect(skills.Status).To(Equal("ok"))
			Expect(skills.Detail).To(ContainSubstring(skill))
		})
	})

	It("prints one line per check, with its fix beneath, as text", func() {
		GinkgoT().Setenv("PATH", GinkgoT().TempDir())
		loginShell("", "")

		var stdout, stderr bytes.Buffer
		code := cli.Run([]string{"doctor", "--output", "text"}, strings.NewReader(""), &stdout, &stderr)

		Expect(code).NotTo(BeZero(), "a failed check fails the run")
		Expect(stdout.String()).To(MatchRegexp(`(?m)^fail\s+path\s+neither this shell`))
		Expect(stdout.String()).To(MatchRegexp(`(?m)^\s+fix: .*shims`))
		Expect(stdout.String()).To(MatchRegexp(`(?m)^ok\s+binary\s+skift `))
	})
})
