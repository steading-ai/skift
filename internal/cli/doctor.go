package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/client"
	"github.com/steading-ai/skift/internal/version"
)

// skift doctor checks a workstation the way a fresh shell and a fresh
// agent session will find it: the binary on PATH, the configuration, the
// service, the MCP server and the skills. Each check says what it found
// and, when something is wrong, what fixes it, so a person or an agent can
// act on the report without tracing the failure themselves.

// The states a check ends in.
const (
	doctorOK   = "ok"
	doctorWarn = "warn"
	doctorFail = "fail"
)

// doctorTimeout bounds each check that waits on another process or the
// service.
const doctorTimeout = 10 * time.Second

// The pages a fix points at. They are the canonical docs for each tool, so
// skift never restates how mise or a shell is set up.
const (
	docInstall     = "https://skift.stead.ing/docs/install/"
	docAgentSkills = "https://skift.stead.ing/docs/agent-skills/"
	docMiseShims   = "https://mise.jdx.dev/dev-tools/shims.html"
)

// doctorCheck is one line of the report.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// doctorReport is the whole report, as JSON prints it.
type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that skift, its service, the MCP server and the skills are set up",
		Long: `Check this workstation the way a new shell and a new agent session find it:

  binary   this skift, and its version
  path     whether a login shell finds skift on PATH, as a harness
           starting "skift mcp" needs it to
  config   the config file, and the socket or address the CLI uses
  service  whether the service answers, and accepts this caller
  mcp      whether "skift mcp" starts and lists its tools
  skills   whether the skift skill is installed where a harness reads it

Each check is ok, warn or fail, and a check that is not ok says what fixes
it. The exit status is non-zero when any check fails. Run it after
installing, and whenever an agent says it cannot reach skift.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDoctor(cmd)
		},
	}
}

func (a *app) runDoctor(cmd *cobra.Command) error {
	p, err := a.printer(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	self, selfErr := os.Executable()

	report := doctorReport{OK: true}
	add := func(c doctorCheck) {
		if c.Status == doctorFail {
			report.OK = false
		}
		report.Checks = append(report.Checks, c)
	}

	add(checkBinary(self, selfErr))
	add(checkPath(ctx, self))
	_, cfgCheck := a.checkConfig()
	add(cfgCheck)
	if cfgCheck.Status == doctorFail {
		add(doctorCheck{Name: "service", Status: doctorFail, Detail: "not checked: the configuration does not load", Fix: "fix the config check first"})
	} else {
		add(a.checkService(ctx))
	}
	add(a.checkMCP(ctx, self, selfErr))
	add(checkSkills())

	if err := p.emitDoctor(report); err != nil {
		return err
	}
	if !report.OK {
		failed := 0
		for _, c := range report.Checks {
			if c.Status == doctorFail {
				failed++
			}
		}
		return &Error{Code: connect.CodeFailedPrecondition.String(), Message: fmt.Sprintf("skift doctor: %d of %d checks failed", failed, len(report.Checks))}
	}
	return nil
}

// emitDoctor prints the report: one line per check, with its fix beneath,
// on a terminal; the report object as JSON otherwise.
func (p printer) emitDoctor(r doctorReport) error {
	if p.format == OutputJSON {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = p.w.Write(append(raw, '\n'))
		return err
	}
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-4s  %-7s  %s\n", c.Status, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(&b, "                 fix: %s\n", c.Fix)
		}
	}
	_, err := io.WriteString(p.w, b.String())
	return err
}

func checkBinary(self string, err error) doctorCheck {
	c := doctorCheck{Name: "binary", Status: doctorOK, Detail: "skift " + version.String()}
	if err == nil {
		c.Detail += " at " + self
	}
	return c
}

// checkPath asks a login shell where skift is. A harness starts "skift
// mcp" by name, from an environment much like that shell's and unlike the
// interactive one the install ran in, so this is the check that catches a
// tool manager activated only for interactive shells.
func checkPath(ctx context.Context, self string) doctorCheck {
	c := doctorCheck{Name: "path"}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-l", "-c", "command -v skift").Output()
	found := lastLine(string(out))
	if err != nil || found == "" {
		c.Status = doctorFail
		c.Detail = fmt.Sprintf("a login shell (%s -l) does not find skift on PATH, so a harness cannot start skift mcp", shell)
		c.Fix = "put the directory holding skift on PATH for login shells, not only interactive ones; with mise, use its shims: " + docMiseShims
		return c
	}
	c.Status = doctorOK
	c.Detail = "a login shell finds " + found
	if self != "" && !sameFile(found, self) {
		c.Status = doctorWarn
		c.Detail = fmt.Sprintf("a login shell finds %s, not this skift at %s", found, self)
		c.Fix = "remove the other copy, or put the one you mean to use first on PATH, so agents and you run the same version"
	}
	return c
}

// lastLine is the last non-blank line of out: a shell's profile can print
// before the command runs.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// sameFile reports whether a and b are one file, following links: a mise
// shim and the binary it runs are not, which is reported, but a symlink to
// this binary is.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// checkConfig loads the configuration and says where the transport came
// from. An address set only in the environment is a warning: a harness
// started outside the shell does not see it.
func (a *app) checkConfig() (client.Config, doctorCheck) {
	c := doctorCheck{Name: "config"}
	path := a.configPath
	if path == "" {
		path = os.Getenv(client.EnvConfig)
	}
	if path == "" {
		path = client.DefaultConfigPath()
	}
	cfg, err := a.config()
	if err != nil {
		c.Status = doctorFail
		c.Detail = err.Error()
		c.Fix = "correct or remove " + path + "; it holds a JSON object such as {\"address\": \"http://skift.<tailnet>.ts.net\"}"
		return cfg, c
	}
	file := path + " (absent)"
	var fromFile client.Config
	if raw, err := os.ReadFile(path); err == nil {
		file = path
		_ = json.Unmarshal(raw, &fromFile)
	}

	c.Status = doctorOK
	switch {
	case cfg.Address != "":
		source := "the config file"
		switch {
		case a.flags.Address != "":
			source = "--address"
		case os.Getenv(client.EnvAddress) != "":
			source = "$" + client.EnvAddress
		}
		c.Detail = fmt.Sprintf("address %s, from %s; config file %s", cfg.Address, source, file)
		if source == "$"+client.EnvAddress && fromFile.Address == "" {
			c.Status = doctorWarn
			c.Fix = fmt.Sprintf("a harness that starts skift mcp outside your shell does not see $%s; put {\"address\": %q} in %s", client.EnvAddress, cfg.Address, path)
		}
	default:
		socket := cfg.Socket
		source := "--socket"
		switch {
		case a.flags.Socket != "":
		case os.Getenv(client.EnvSocket) != "":
			source = "$" + client.EnvSocket
		case socket != "":
			source = "the config file"
		default:
			socket = client.DefaultSocketPath()
			source = "the default"
		}
		c.Detail = fmt.Sprintf("socket %s, from %s; config file %s", socket, source, file)
	}
	return cfg, c
}

// checkService asks the service for one project: it answers only a caller
// it has mapped to a principal, so this proves both the transport and the
// identity.
func (a *app) checkService(ctx context.Context) doctorCheck {
	c := doctorCheck{Name: "service"}
	cl, err := a.connect()
	if err != nil {
		c.Status = doctorFail
		c.Detail = err.Error()
		c.Fix = "correct the address or socket the config check reports"
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	res, err := cl.Graph.ListProjects(ctx, &skiftv1.ListProjectsRequest{PageSize: 1})
	if err == nil {
		c.Status = doctorOK
		c.Detail = fmt.Sprintf("the service at %s answered", cl.Target())
		if len(res.GetProjects()) == 0 {
			c.Detail += "; you can read no projects yet"
		}
		return c
	}
	c.Status = doctorFail
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		c.Detail = fmt.Sprintf("the service at %s answered, but does not know this caller", cl.Target())
		c.Fix = "bind this machine to a principal on the server with skift admin bootstrap, as " + docInstall + "#create-the-first-principal describes"
	case connect.CodePermissionDenied:
		c.Detail = fmt.Sprintf("the service at %s answered, but refused this caller: %s", cl.Target(), ToError(err).Message)
		c.Fix = "ask whoever administers the service to grant your principal access"
	default:
		c.Detail = fmt.Sprintf("cannot reach the service at %s: %s", cl.Target(), ToError(err).Message)
		if cl.Transport() == client.TransportHTTP {
			c.Fix = "check that this machine is on the tailnet (tailscale status) and that the address is the server's"
		} else {
			c.Fix = "start the service, or point skift at one on another machine with an address in " + client.DefaultConfigPath() + "; see " + docInstall
		}
	}
	return c
}

// checkMCP starts this binary's MCP server as a harness would and lists
// its tools. The connection flags given to doctor are passed on, so the
// server is checked against the same service.
func (a *app) checkMCP(ctx context.Context, self string, selfErr error) doctorCheck {
	c := doctorCheck{Name: "mcp"}
	if selfErr != nil {
		c.Status = doctorFail
		c.Detail = "cannot find this binary to start it: " + selfErr.Error()
		return c
	}
	var args []string
	if a.configPath != "" {
		args = append(args, "--config", a.configPath)
	}
	if a.flags.Socket != "" {
		args = append(args, "--socket", a.flags.Socket)
	}
	if a.flags.Address != "" {
		args = append(args, "--address", a.flags.Address)
	}
	args = append(args, "mcp")

	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcpProtocolVersion + `","capabilities":{},"clientInfo":{"name":"skift-doctor","version":"` + version.String() + `"}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	out, err := cmd.Output()

	tools := -1
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for scanner.Scan() {
		var res struct {
			ID     int `json:"id"`
			Result struct {
				Tools []json.RawMessage `json:"tools"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &res) == nil && res.ID == 2 {
			tools = len(res.Result.Tools)
		}
	}
	if tools > 0 {
		c.Status = doctorOK
		c.Detail = fmt.Sprintf("skift mcp started and listed %d tools", tools)
		return c
	}
	c.Status = doctorFail
	why := strings.TrimSpace(stderr.String())
	switch {
	case why != "":
		why = lastLine(why)
	case err != nil:
		why = err.Error()
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		why = "it did not answer in time"
	default:
		why = "it listed no tools"
	}
	c.Detail = "skift mcp did not start: " + why
	c.Fix = "fix the checks above; skift mcp reads the same configuration as the CLI"
	return c
}

// checkSkills looks for the skift skill where the plugin and the common
// installers put it. Finding none is a warning: an agent can still drive
// the CLI, it only has not been taught how.
func checkSkills() doctorCheck {
	c := doctorCheck{Name: "skills"}
	var places []string
	if home, err := os.UserHomeDir(); err == nil {
		places = append(places,
			filepath.Join(home, ".claude", "plugins", "cache", "*", "skift", "*", "skills", "skift", "SKILL.md"),
			filepath.Join(home, ".claude", "skills", "skift", "SKILL.md"),
			filepath.Join(home, ".agents", "skills", "skift", "SKILL.md"),
		)
	}
	places = append(places,
		filepath.Join(".claude", "skills", "skift", "SKILL.md"),
		filepath.Join(".agents", "skills", "skift", "SKILL.md"),
	)
	var found []string
	for _, pattern := range places {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			found = append(found, filepath.Dir(m))
		}
	}
	if len(found) == 0 {
		c.Status = doctorWarn
		c.Detail = "no skift skill found in the Claude Code plugin cache, ~/.claude/skills, ~/.agents/skills, or this directory"
		c.Fix = "install the plugin or the skills, as " + docAgentSkills + " describes; a harness that keeps skills elsewhere can ignore this"
		return c
	}
	c.Status = doctorOK
	c.Detail = "the skift skill is installed at " + strings.Join(found, ", ")
	return c
}
