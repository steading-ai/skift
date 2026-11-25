package cli

import (
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
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// doctorExecutable is the binary the mcp check starts. It is a variable
// because in a spec this process is the test binary, not skift.
var doctorExecutable = os.Executable

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
  path     whether this shell and a login shell find skift on PATH, and
           the same version of it, as a harness starting "skift mcp" needs
  config   the config file, and the socket or address the CLI uses
  service  whether the service answers, and accepts this caller
  mcp      whether "skift mcp" starts, lists its tools, and answers one
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
	self, selfErr := doctorExecutable()

	report := doctorReport{OK: true}
	add := func(c doctorCheck) {
		if c.Status == doctorFail {
			report.OK = false
		}
		report.Checks = append(report.Checks, c)
	}

	add(checkBinary(self, selfErr))
	add(checkPath(ctx))
	cfgCheck := a.checkConfig()
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

// foundSkift is a skift found by name, and the version it reports. Version is
// empty when it could not be asked.
type foundSkift struct {
	path, version string
}

func (f foundSkift) String() string {
	if f.version == "" {
		return f.path + " (which did not report its version)"
	}
	return fmt.Sprintf("skift %s at %s", f.version, f.path)
}

// versionPrefix starts what "skift version" prints.
const versionPrefix = "skift version "

// checkPath looks for skift by name where a harness does. A harness
// started from a terminal inherits that terminal's PATH, which is this
// process's; one started from a desktop or a service manager gets
// something closer to a login shell's, which does not read the files an
// interactive shell does. Each copy found is asked its version, because a
// tool manager's shim is a different file from the binary it runs.
func checkPath(ctx context.Context) doctorCheck {
	c := doctorCheck{Name: "path"}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	here, login := findHere(ctx), findLogin(ctx, shell)
	shims := "with mise, put its shims on PATH for login shells: " + docMiseShims

	switch {
	case here == nil && login == nil:
		c.Status = doctorFail
		c.Detail = fmt.Sprintf("neither this shell nor a login shell (%s -l) finds skift on PATH, so no harness can start skift mcp", shell)
		c.Fix = "put the directory holding skift on PATH for login shells as well as interactive ones; " + shims
		return c
	case login == nil:
		c.Status = doctorWarn
		c.Detail = fmt.Sprintf("this shell finds %s, but a login shell (%s -l) finds no skift, so a harness started outside a terminal cannot start skift mcp", here, shell)
		c.Fix = "put the directory holding skift on PATH for login shells too; " + shims
		return c
	case here == nil:
		c.Status = doctorWarn
		c.Detail = fmt.Sprintf("a login shell finds %s, but this shell's PATH has no skift, so a harness started from this terminal cannot start skift mcp", login)
		c.Fix = "open a new terminal, or put the directory holding skift on this shell's PATH"
		return c
	}

	for _, f := range []*foundSkift{here, login} {
		if f.version != version.String() {
			c.Status = doctorWarn
			c.Detail = fmt.Sprintf("this is skift %s, but %s is what a harness starts", version.String(), f)
			c.Fix = "remove the other copy, or put the one you mean to use first on PATH, so agents and you run the same version"
			return c
		}
	}
	c.Status = doctorOK
	c.Detail = "this shell and a login shell find " + login.String()
	if here.path != login.path {
		c.Detail += ", also at " + here.path
	}
	return c
}

// findHere finds skift on this process's PATH and asks its version.
func findHere(ctx context.Context) *foundSkift {
	path, err := exec.LookPath("skift")
	if err != nil {
		return nil
	}
	f := &foundSkift{path: path}
	if out, err := exec.CommandContext(ctx, path, "version").Output(); err == nil {
		f.version = strings.TrimPrefix(lastLine(string(out)), versionPrefix)
	}
	return f
}

// findLogin asks a login shell where skift is and its version. The
// shell's profile may print before either answer, so the answers are the
// last path the shell prints and the last version line.
func findLogin(ctx context.Context, shell string) *foundSkift {
	out, _ := exec.CommandContext(ctx, shell, "-l", "-c", "command -v skift; skift version").Output()
	var f foundSkift
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, versionPrefix):
			f.version = strings.TrimPrefix(line, versionPrefix)
		case filepath.IsAbs(line):
			f.path = line
		}
	}
	if f.path == "" {
		return nil
	}
	return &f
}

// lastLine is the last non-blank line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// sourceName names a source of a connection setting as a person sets it.
func sourceName(from client.Source, flag, env string) string {
	switch from {
	case client.SourceFlag:
		return "--" + flag
	case client.SourceEnv:
		return "$" + env
	case client.SourceFile:
		return "the config file"
	default:
		return "the default"
	}
}

// checkConfig loads the configuration and says where the transport came
// from. A socket or address set only in the environment is a warning: a
// harness started outside the shell does not see it.
func (a *app) checkConfig() doctorCheck {
	c := doctorCheck{Name: "config"}
	l, err := a.loaded()
	file := l.Path
	switch {
	case file == "":
		file = "(none: no home directory)"
	case !l.Found:
		file += " (absent)"
	}
	if err != nil {
		c.Status = doctorFail
		c.Detail = err.Error()
		c.Fix = "correct or remove " + l.Path + "; it holds a JSON object such as {\"address\": \"http://skift.<tailnet>.ts.net\"}"
		return c
	}

	c.Status = doctorOK
	key, value, from := "socket", l.Socket, sourceName(l.SocketFrom, "socket", client.EnvSocket)
	envOnly := l.SocketFrom == client.SourceEnv
	if l.Address != "" {
		key, value, from = "address", l.Address, sourceName(l.AddressFrom, "address", client.EnvAddress)
		envOnly = l.AddressFrom == client.SourceEnv
	} else if l.SocketFrom == client.SourceDefault {
		value = client.DefaultSocketPath()
	}
	c.Detail = fmt.Sprintf("%s %s, from %s; config file %s", key, value, from, file)
	if envOnly {
		c.Status = doctorWarn
		c.Fix = fmt.Sprintf("a harness that starts skift mcp outside your shell does not see %s; put {%q: %q} in %s", from, key, value, l.Path)
	}
	return c
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
			l, _ := a.loaded()
			c.Fix = "start the service, or point skift at one on another machine with an address in " + l.Path + "; see " + docInstall
		}
	}
	return c
}

// checkMCP starts this binary's MCP server as a harness would, lists its
// tools and calls project_list, the one tool that needs nothing but a
// service. The connection flags given to doctor are passed on, so the
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
	tools, called, err := probeMCP(ctx, self, args)
	c.Status = doctorFail
	c.Fix = "fix the checks above; skift mcp reads the same configuration as the CLI"
	switch {
	case err != nil:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("it did not answer within %s", doctorTimeout)
		}
		c.Detail = "skift mcp did not start: " + err.Error()
	case tools == 0:
		c.Detail = "skift mcp started but listed no tools"
	case called != "":
		c.Detail = fmt.Sprintf("skift mcp started and listed %d tools, but project_list failed: %s", tools, called)
	default:
		c.Status, c.Fix = doctorOK, ""
		c.Detail = fmt.Sprintf("skift mcp started, listed %d tools, and answered project_list", tools)
	}
	return c
}

// probeMCP runs an MCP session over stdio as a harness does: initialize,
// tools/list, then a project_list call. called is the tool's error text,
// empty when it succeeded.
func probeMCP(ctx context.Context, self string, args []string) (tools int, called string, err error) {
	cmd := exec.CommandContext(ctx, self, args...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	// why is the server's own account of a failure, when it gave one.
	why := func(err error) error {
		if line := lastLine(stderr.String()); line != "" {
			return errors.New(line)
		}
		return err
	}

	c := mcp.NewClient(&mcp.Implementation{Name: "skift-doctor", Version: version.String()}, nil)
	session, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return 0, "", why(err)
	}
	defer func() { _ = session.Close() }()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		return 0, "", why(err)
	}
	if len(list.Tools) == 0 {
		return 0, "", nil
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "project_list", Arguments: map[string]any{}})
	if err != nil {
		return len(list.Tools), "", why(err)
	}
	if !res.IsError {
		return len(list.Tools), "", nil
	}
	text := "the tool reported an error"
	for _, content := range res.Content {
		if t, ok := content.(*mcp.TextContent); ok {
			text = strings.SplitN(t.Text, "\n", 2)[0]
			break
		}
	}
	return len(list.Tools), text, nil
}

// syncBuffer is a buffer a process writes while doctor may read it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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
