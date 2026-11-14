package cli

import (
	"github.com/spf13/cobra"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/codec"
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
	def   skiftv1.View
}

func (f *viewFlag) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.value, "view", viewName(f.def), viewHelp)
}

// view is the flag's value as the wire enum.
func (f *viewFlag) view() (skiftv1.View, error) {
	return parseView("--view", f.value, f.def)
}

// parseView reads a short view name such as "summary". Empty takes def.
func parseView(name, value string, def skiftv1.View) (skiftv1.View, error) {
	v, err := parseEnum[skiftv1.View](name, value)
	if err != nil {
		return 0, err
	}
	if v == skiftv1.View_VIEW_UNSPECIFIED {
		return def, nil
	}
	return v, nil
}

// viewName is the short name of a view: "summary" for VIEW_SUMMARY.
func viewName(v skiftv1.View) string {
	return codec.EnumName(v)
}
