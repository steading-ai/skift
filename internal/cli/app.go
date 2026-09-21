package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ivy/dagger/internal/client"
	"github.com/ivy/dagger/internal/codec"
)

// app is the state the CLI commands share: the global flags, the resolved
// configuration, and the client, built on first use so that commands that
// never call the service (help, version, serve) never read a config file.
type app struct {
	configPath string
	// flags are the connection values given on the command line, laid over
	// the config file and environment.
	flags  client.Config
	output string
	// projects are the --project values given on the command line, zero or
	// more, each a project filter by slug, org/slug, or id. The config file
	// and $DG_PROJECT still name a single default project, laid under
	// these, and empty means every project the caller may read.
	projects []string

	cfg    *client.Config
	client *client.Client
}

// config resolves the configuration once: file, then environment, then
// flags.
func (a *app) config() (client.Config, error) {
	if a.cfg == nil {
		cfg, err := client.LoadConfig(a.configPath)
		if err != nil {
			return client.Config{}, err
		}
		cfg = cfg.Override(a.flags)
		a.cfg = &cfg
	}
	return *a.cfg, nil
}

// connect returns the client, building it on first use.
func (a *app) connect() (*client.Client, error) {
	if a.client == nil {
		cfg, err := a.config()
		if err != nil {
			return nil, err
		}
		c, err := client.New(cfg)
		if err != nil {
			return nil, err
		}
		a.client = c
	}
	return a.client, nil
}

// project is the current project, or an error naming how to set one when a
// command needs it and none is configured.
func (a *app) project() (string, error) {
	project, err := a.currentProject()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", usage("a project is required; pass --project, set $%s, or put it in the config file", client.EnvProject)
	}
	return project, nil
}

// currentProjects is every project a command's filter should scope to:
// --project given once or more, else the config file's or $DG_PROJECT's
// single project as the sole default, else empty, meaning every project
// the caller may read.
func (a *app) currentProjects() ([]string, error) {
	if len(a.projects) > 0 {
		return a.projects, nil
	}
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	if cfg.Project == "" {
		return nil, nil
	}
	return []string{cfg.Project}, nil
}

// currentProject is [app.currentProjects] narrowed to at most one, for a
// command that names a single project's address rather than a filter.
// Empty means none is set, for the command whose scope is the
// organization when no project is named rather than an error.
func (a *app) currentProject() (string, error) {
	projects, err := a.currentProjects()
	if err != nil {
		return "", err
	}
	switch len(projects) {
	case 0:
		return "", nil
	case 1:
		return projects[0], nil
	default:
		return "", usage("only one --project is allowed here; got %d", len(projects))
	}
}

// projectFor is the project a request about refs resolves in: the current
// project when one is set, else the project the references themselves
// carry, which every qualified reference does.
func (a *app) projectFor(refs ...client.NodeRef) (string, error) {
	project, err := a.currentProject()
	if err != nil {
		return "", err
	}
	if project != "" {
		return project, nil
	}
	for _, r := range refs {
		if p := r.ProjectRef(); p != "" {
			return p, nil
		}
	}
	return "", usage("a bare #number reference needs a project; pass --project, set $%s, or qualify the reference as slug#number", client.EnvProject)
}

// printer returns the output writer for cmd in the format the flags and
// the terminal select.
func (a *app) printer(cmd *cobra.Command) (printer, error) {
	format := a.output
	switch format {
	case "":
		format = OutputJSON
		if isTerminal(cmd.OutOrStdout()) {
			format = OutputText
		}
	case OutputJSON, OutputText:
	default:
		return printer{}, usage("--output must be json or text, not %q", format)
	}
	return printer{w: cmd.OutOrStdout(), format: format}, nil
}

// parseRef parses one node reference argument as a usage error.
func parseRef(s string) (client.NodeRef, error) {
	ref, err := client.ParseNodeRef(s)
	if err != nil {
		return client.NodeRef{}, &usageError{err: err}
	}
	return ref, nil
}

// readBody returns arg, or the whole of stdin when arg is "-".
func readBody(cmd *cobra.Command, arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(raw), nil
}

// protoEnum is a generated wire enum: an int32 that knows its own
// descriptor, which is where every spelling of its values comes from.
type protoEnum interface {
	~int32
	Descriptor() protoreflect.EnumDescriptor
}

// enumFlag parses the value of a flag into a wire enum. The short form
// ("p1", "user", "crash"), the unprefixed name, and the canonical name are
// all accepted, the same three spellings the JSON codec takes. Empty means
// unspecified, which the server treats as its default.
func enumFlag[E protoEnum](flag, value string) (E, error) {
	return parseEnum[E]("--"+flag, value)
}

// requiredEnumFlag is enumFlag for a flag whose presence states a value:
// the empty string is a mistake rather than "leave it to the default".
func requiredEnumFlag[E protoEnum](flag, value string) (E, error) {
	var zero E
	v, err := enumFlag[E](flag, value)
	if err != nil {
		return zero, err
	}
	if v == zero {
		return zero, usage("--%s: %q is not one of %s", flag, value, strings.Join(codec.EnumNames(zero.Descriptor()), ", "))
	}
	return v, nil
}

// parseEnum is enumFlag for an input named by something other than a flag,
// such as an MCP tool argument. The name is how the error refers to it.
func parseEnum[E protoEnum](name, value string) (E, error) {
	var zero E
	if value == "" {
		return zero, nil
	}
	ed := zero.Descriptor()
	v, ok := codec.Value(ed, value)
	if !ok || v.Number() == 0 {
		return zero, usage("%s: %q is not one of %s", name, value, strings.Join(codec.EnumNames(ed), ", "))
	}
	return E(v.Number()), nil
}

// enumChoices lists the short names of an enum's non-zero values, sorted:
// the legal values of a flag or of a tool schema.
func enumChoices[E protoEnum]() []string {
	var zero E
	return codec.EnumNames(zero.Descriptor())
}

// shortNames is the short name of each of a chosen few enum values, in the
// order given, for a schema that offers a subset.
func shortNames[E protoreflect.Enum](values ...E) []string {
	names := make([]string, 0, len(values))
	for _, v := range values {
		names = append(names, codec.EnumName(v))
	}
	return names
}

// send runs one RPC and prints its response.
func send[Res proto.Message](a *app, cmd *cobra.Command, call func() (Res, error)) error {
	p, err := a.printer(cmd)
	if err != nil {
		return err
	}
	res, err := call()
	if err != nil {
		return err
	}
	return p.emit(res)
}
