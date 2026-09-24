package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/codec"
)

// Output formats. JSON is what an agent parses and the default whenever
// stdout is not a terminal; text is for a person at one.
const (
	OutputJSON = "json"
	OutputText = "text"
)

// isTerminal reports whether w is a character device, which is how the
// default output format is chosen.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// printer writes responses in the chosen format.
type printer struct {
	w      io.Writer
	format string
}

// emit writes one response. In JSON mode it is one compact object on one
// line, so several emits (an event stream) are newline-delimited JSON.
func (p printer) emit(msg proto.Message) error {
	if p.format == OutputText {
		return p.text(msg)
	}
	raw, err := codec.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	_, err = fmt.Fprintf(p.w, "%s\n", raw)
	return err
}

// text renders the responses a person reads most as lines, and everything
// else as indented JSON.
func (p printer) text(msg proto.Message) error {
	var b strings.Builder
	switch m := msg.(type) {
	case *skiftv1.ReadyResponse:
		writeNodes(&b, m.GetNodes())
		if len(m.GetNodes()) == 0 && m.GetHeldCount() > 0 {
			fmt.Fprintf(&b, "nothing ready; %d open nodes are held\n", m.GetHeldCount())
		}
		writePageToken(&b, m.GetNextPageToken())
	case *skiftv1.ListNodesResponse:
		writeNodes(&b, m.GetNodes())
		writePageToken(&b, m.GetNextPageToken())
	case *skiftv1.ApplyPatchResponse:
		writeNodes(&b, m.GetNodes())
		for alias, ref := range m.GetAliases() {
			fmt.Fprintf(&b, "alias %s = %s\n", alias, ref)
		}
		fmt.Fprintf(&b, "%d events, request %s\n", m.GetEventCount(), m.GetRequestId())
	case *skiftv1.GetNodeResponse:
		writeNodes(&b, []*skiftv1.Node{m.GetNode()})
		if body := m.GetNode().GetBody(); body != "" {
			fmt.Fprintf(&b, "\n%s\n", body)
		}
		for _, e := range m.GetEdges() {
			fmt.Fprintf(&b, "edge %s -%s-> %s\n", e.GetFrom(), codec.EnumName(e.GetKind()), e.GetTo())
		}
		if l := m.GetLease(); l != nil {
			writeLease(&b, l)
		}
		writeUsage(&b, m.GetEstimate(), m.GetUsage())
	case *skiftv1.ReportUsageResponse:
		writeUsageRecord(&b, "reported", m.GetUsage())
	case *skiftv1.GetInputsResponse:
		if len(m.GetInputs()) == 0 {
			b.WriteString("no inputs\n")
		}
		for _, in := range m.GetInputs() {
			writeNodes(&b, []*skiftv1.Node{in.GetNode()})
			fmt.Fprintf(&b, "    %d bytes\n", in.GetBytes())
			if body := in.GetResult().GetBody(); body != "" {
				fmt.Fprintf(&b, "\n%s\n\n", body)
			}
		}
	case *skiftv1.ExplainResponse:
		writeNodes(&b, []*skiftv1.Node{m.GetNode()})
		if len(m.GetReasons()) == 0 {
			b.WriteString("ready\n")
		}
		for _, r := range m.GetReasons() {
			writeReason(&b, r)
		}
	case *skiftv1.ClaimNextResponse:
		if m.GetNode() == nil {
			b.WriteString("nothing to claim\n")
			break
		}
		writeNodes(&b, []*skiftv1.Node{m.GetNode()})
		writeLease(&b, m.GetLease())
	case *skiftv1.HeartbeatResponse:
		writeLease(&b, m.GetLease())
	case *skiftv1.ReleaseResponse:
		writeLease(&b, m.GetLease())
	case *skiftv1.ListProjectsResponse:
		for _, p := range m.GetProjects() {
			writeProject(&b, p)
		}
		writePageToken(&b, m.GetNextPageToken())
	case *skiftv1.CreateProjectResponse:
		writeProject(&b, m.GetProject())
	case *skiftv1.GetProjectResponse:
		writeProject(&b, m.GetProject())
	case *skiftv1.UpdateProjectResponse:
		writeProject(&b, m.GetProject())
	case *skiftv1.ArchiveProjectResponse:
		writeProject(&b, m.GetProject())
	case *skiftv1.HoldProjectResponse:
		writeProject(&b, m.GetProject())
		fmt.Fprintf(&b, "%d open nodes left the ready set\n", m.GetHeldCount())
	case *skiftv1.UnholdProjectResponse:
		writeProject(&b, m.GetProject())
	case *skiftv1.FocusResponse:
		fmt.Fprintf(&b, "focus %s: %d held, %d released\n", m.GetGroup(), m.GetHeldCount(), m.GetReleasedCount())
	case *skiftv1.ClearFocusResponse:
		fmt.Fprintf(&b, "focus cleared: %d released\n", m.GetReleasedCount())
	case *skiftv1.SearchResponse:
		for _, h := range m.GetHits() {
			writeNodes(&b, []*skiftv1.Node{h.GetNode()})
			if s := h.GetSnippet(); s != "" {
				fmt.Fprintf(&b, "    %s\n", s)
			}
		}
		writePageToken(&b, m.GetNextPageToken())
	default:
		raw, err := codec.MarshalIndent(msg)
		if err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	_, err := io.WriteString(p.w, b.String())
	return err
}

// writeNodes renders one line per node. A node in the refs view has no
// title to show, so its line is its number, state, and reference.
func writeNodes(b *strings.Builder, nodes []*skiftv1.Node) {
	for _, n := range nodes {
		state := codec.EnumName(n.GetStatus())
		switch {
		case n.GetClaimed():
			state = "claimed"
		case n.GetReady():
			state = "ready"
		case n.GetHeld():
			state = "held"
		case n.GetBlocked():
			state = "blocked"
		}
		if n.GetTitle() == "" && n.GetKind() == "" {
			fmt.Fprintf(b, "#%-5d %-8s %s\n", n.GetNumber(), state, n.GetReference())
			continue
		}
		fmt.Fprintf(b, "#%-5d %-3s %-8s %-6s %s", n.GetNumber(), codec.EnumName(n.GetPriority()), state, n.GetKind(), n.GetTitle())
		if len(n.GetLabels()) > 0 {
			fmt.Fprintf(b, "  [%s]", strings.Join(n.GetLabels(), ", "))
		}
		b.WriteByte('\n')
	}
}

func writeProject(b *strings.Builder, p *skiftv1.Project) {
	fmt.Fprintf(b, "%-24s %s", p.GetSlug(), p.GetTitle())
	if url := p.GetVcsUrl(); url != "" {
		fmt.Fprintf(b, "  %s", url)
	}
	if p.GetArchivedAt() != nil {
		b.WriteString("  (archived)")
	}
	if h := p.GetHold(); h != nil {
		fmt.Fprintf(b, "  (held: %s)", h.GetReason())
	}
	b.WriteByte('\n')
}

func writeLease(b *strings.Builder, l *skiftv1.Lease) {
	fmt.Fprintf(b, "lease %s", l.GetId())
	if l.GetReleaseReason() != skiftv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED {
		fmt.Fprintf(b, " released: %s\n", codec.EnumName(l.GetReleaseReason()))
		return
	}
	fmt.Fprintf(b, " expires in %s\n", time.Until(l.GetExpiresAt().AsTime()).Round(time.Second))
}

// writeUsage renders what a node is expected to cost and what it and its
// descendants have cost, one line per model. Counts, never currency.
func writeUsage(b *strings.Builder, estimate *skiftv1.Usage, rollup *skiftv1.Rollup) {
	if estimate != nil {
		writeUsageRecord(b, "estimate", estimate)
	}
	for _, t := range rollup.GetActual() {
		fmt.Fprintf(b, "cost     %s  (%d records)\n", tokens(t.GetModel(), t.GetInputTokens(), t.GetOutputTokens(), t.GetCacheReadTokens(), t.GetCacheWriteTokens()), t.GetRecords())
	}
	for _, t := range rollup.GetEstimate() {
		fmt.Fprintf(b, "expected %s  (%d nodes)\n", tokens(t.GetModel(), t.GetInputTokens(), t.GetOutputTokens(), t.GetCacheReadTokens(), t.GetCacheWriteTokens()), t.GetRecords())
	}
}

// writeUsageRecord renders one usage record under a label.
func writeUsageRecord(b *strings.Builder, label string, u *skiftv1.Usage) {
	fmt.Fprintf(b, "%-8s %s", label, tokens(u.GetModel(), u.GetInputTokens(), u.GetOutputTokens(), u.GetCacheReadTokens(), u.GetCacheWriteTokens()))
	if basis := u.GetBasis(); basis != "" {
		fmt.Fprintf(b, "  basis: %s", basis)
	}
	b.WriteByte('\n')
}

// tokens renders the four counts of a usage record, named so a reader
// need not remember their order. A record naming no model shows a dash.
func tokens(model string, input, output, cacheRead, cacheWrite int64) string {
	if model == "" {
		model = "-"
	}
	return fmt.Sprintf("%-20s %d in, %d out, %d cache read, %d cache write", model, input, output, cacheRead, cacheWrite)
}

func writeReason(b *strings.Builder, r *skiftv1.Reason) {
	switch x := r.GetReason().(type) {
	case *skiftv1.Reason_NotOpen:
		fmt.Fprintf(b, "not open: %s\n", codec.EnumName(x.NotOpen.GetStatus()))
	case *skiftv1.Reason_Blocked:
		gate := ""
		if x.Blocked.GetGate() {
			gate = " (gate)"
		}
		fmt.Fprintf(b, "blocked by #%d %s%s\n", x.Blocked.GetBlocker().GetNumber(), x.Blocked.GetBlocker().GetTitle(), gate)
	case *skiftv1.Reason_Leased:
		fmt.Fprintf(b, "leased by %s until %s\n", x.Leased.GetLease().GetHolder(), x.Leased.GetLease().GetExpiresAt().AsTime().Format(time.RFC3339))
	case *skiftv1.Reason_OpenChild:
		fmt.Fprintf(b, "open child #%d %s\n", x.OpenChild.GetChild().GetNumber(), x.OpenChild.GetChild().GetTitle())
	case *skiftv1.Reason_Archived:
		fmt.Fprintf(b, "project %s is archived\n", x.Archived.GetProject().GetSlug())
	case *skiftv1.Reason_ProjectHeld:
		fmt.Fprintf(b, "project %s is held: %s\n", x.ProjectHeld.GetProject().GetSlug(), x.ProjectHeld.GetProject().GetHold().GetReason())
	case *skiftv1.Reason_Held:
		where := fmt.Sprintf("held by #%d %s", x.Held.GetNumber(), x.Held.GetTitle())
		if x.Held.GetSelf() {
			where = "held"
		}
		fmt.Fprintf(b, "%s since %s: %s\n", where,
			x.Held.GetHold().GetAt().AsTime().Format(time.RFC3339), x.Held.GetHold().GetReason())
	}
}

func writePageToken(b *strings.Builder, token string) {
	if token != "" {
		fmt.Fprintf(b, "next page: --page-token %s\n", token)
	}
}
