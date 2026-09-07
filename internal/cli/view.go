package cli

import (
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

// The view an argument selects: how much of each node a response carries.
// The service applies one projection for every RPC; this is the client's
// end of it, sending the default the command wants explicitly so the wire
// says what the caller asked for.

// viewHelp describes the views for a flag or a tool argument.
const viewHelp = `how much of each node to return: refs (reference, status, and flags), summary (plus title, kind, priority, labels), or full (everything, body included)`

// viewFlag is a --view flag with a per-command default.
type viewFlag struct {
	value string
	def   daggerv1.View
}

func (f *viewFlag) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.value, "view", viewName(f.def), viewHelp)
}

// view is the flag's value as the wire enum.
func (f *viewFlag) view() (daggerv1.View, error) {
	return parseView("--view", f.value, f.def)
}

// parseView reads a short view name such as "summary". Empty takes def.
func parseView(name, value string, def daggerv1.View) (daggerv1.View, error) {
	v, err := parseEnum[daggerv1.View](name, value, "VIEW_", daggerv1.View_value)
	if err != nil {
		return 0, err
	}
	if v == daggerv1.View_VIEW_UNSPECIFIED {
		return def, nil
	}
	return v, nil
}

// viewName is the short name of a view: "summary" for VIEW_SUMMARY.
func viewName(v daggerv1.View) string {
	return enumName(v.String())
}

// viewProp is the MCP schema of a view argument defaulting to def.
func viewProp(def daggerv1.View) map[string]any {
	return enumProp(strings.ToUpper(viewHelp[:1])+viewHelp[1:]+". Defaults to "+viewName(def)+".", enumNames(daggerv1.View_value))
}

// splitView takes a short-named view out of raw tool arguments that are
// otherwise a proto message in JSON, where the enum would have to be
// spelt VIEW_FULL. The rest of the arguments are returned for protojson.
func splitView(raw json.RawMessage, def daggerv1.View) (daggerv1.View, json.RawMessage, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return 0, nil, usage("arguments: %v", err)
	}
	value := ""
	if v, ok := args["view"]; ok {
		if err := json.Unmarshal(v, &value); err != nil {
			return 0, nil, usage("view: %v", err)
		}
		delete(args, "view")
	}
	view, err := parseView("view", value, def)
	if err != nil {
		return 0, nil, err
	}
	rest, err := json.Marshal(args)
	if err != nil {
		return 0, nil, usage("arguments: %v", err)
	}
	return view, rest, nil
}
