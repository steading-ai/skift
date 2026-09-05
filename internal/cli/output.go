package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	daggerv1 "github.com/ivy/dagger/gen/dagger/v1"
)

// Output formats. JSON is what an agent parses and the default whenever
// stdout is not a terminal; text is for a person at one.
const (
	OutputJSON = "json"
	OutputText = "text"
)

// jsonMarshaler renders wire messages with their proto field names, which
// are the names docs/domain-model.md uses, one message per line so a stream
// of them is newline-delimited JSON.
var jsonMarshaler = protojson.MarshalOptions{UseProtoNames: true}

// prettyMarshaler is jsonMarshaler indented, for text mode's fallback.
var prettyMarshaler = protojson.MarshalOptions{UseProtoNames: true, Multiline: true, Indent: "  "}

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
	raw, err := jsonMarshaler.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	// protojson's output is stable but not canonical in whitespace;
	// compact it so a line is a line.
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	_, err = fmt.Fprintln(p.w, buf.String())
	return err
}

// text renders the responses a person reads most as lines, and everything
// else as indented JSON.
func (p printer) text(msg proto.Message) error {
	var b strings.Builder
	switch m := msg.(type) {
	case *daggerv1.ReadyResponse:
		writeNodes(&b, m.GetNodes())
		writePageToken(&b, m.GetNextPageToken())
	case *daggerv1.ListNodesResponse:
		writeNodes(&b, m.GetNodes())
		writePageToken(&b, m.GetNextPageToken())
	case *daggerv1.ApplyPatchResponse:
		writeNodes(&b, m.GetNodes())
		for alias, ref := range m.GetAliases() {
			fmt.Fprintf(&b, "alias %s = %s\n", alias, ref)
		}
	case *daggerv1.GetNodeResponse:
		writeNodes(&b, []*daggerv1.Node{m.GetNode()})
		if body := m.GetNode().GetBody(); body != "" {
			fmt.Fprintf(&b, "\n%s\n", body)
		}
		for _, e := range m.GetEdges() {
			fmt.Fprintf(&b, "edge %s -%s-> %s\n", e.GetFrom(), enumName(e.GetKind().String()), e.GetTo())
		}
		if l := m.GetLease(); l != nil {
			writeLease(&b, l)
		}
	case *daggerv1.ExplainResponse:
		writeNodes(&b, []*daggerv1.Node{m.GetNode()})
		if len(m.GetReasons()) == 0 {
			b.WriteString("ready\n")
		}
		for _, r := range m.GetReasons() {
			writeReason(&b, r)
		}
	case *daggerv1.ClaimNextResponse:
		if m.GetNode() == nil {
			b.WriteString("nothing to claim\n")
			break
		}
		writeNodes(&b, []*daggerv1.Node{m.GetNode()})
		writeLease(&b, m.GetLease())
	case *daggerv1.HeartbeatResponse:
		writeLease(&b, m.GetLease())
	case *daggerv1.ReleaseResponse:
		writeLease(&b, m.GetLease())
	case *daggerv1.ListProjectsResponse:
		for _, p := range m.GetProjects() {
			writeProject(&b, p)
		}
		writePageToken(&b, m.GetNextPageToken())
	case *daggerv1.CreateProjectResponse:
		writeProject(&b, m.GetProject())
	case *daggerv1.GetProjectResponse:
		writeProject(&b, m.GetProject())
	case *daggerv1.ArchiveProjectResponse:
		writeProject(&b, m.GetProject())
	case *daggerv1.SearchResponse:
		for _, h := range m.GetHits() {
			writeNodes(&b, []*daggerv1.Node{h.GetNode()})
			if s := h.GetSnippet(); s != "" {
				fmt.Fprintf(&b, "    %s\n", s)
			}
		}
		writePageToken(&b, m.GetNextPageToken())
	default:
		raw, err := prettyMarshaler.Marshal(msg)
		if err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	_, err := io.WriteString(p.w, b.String())
	return err
}

// enumName strips the proto enum prefix: PRIORITY_P2 becomes p2.
func enumName(s string) string {
	if i := strings.LastIndexByte(s, '_'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

func writeNodes(b *strings.Builder, nodes []*daggerv1.Node) {
	for _, n := range nodes {
		state := enumName(n.GetStatus().String())
		switch {
		case n.GetClaimed():
			state = "claimed"
		case n.GetReady():
			state = "ready"
		case n.GetBlocked():
			state = "blocked"
		}
		fmt.Fprintf(b, "#%-5d %-3s %-8s %-6s %s", n.GetNumber(), enumName(n.GetPriority().String()), state, n.GetKind(), n.GetTitle())
		if len(n.GetLabels()) > 0 {
			fmt.Fprintf(b, "  [%s]", strings.Join(n.GetLabels(), ", "))
		}
		b.WriteByte('\n')
	}
}

func writeProject(b *strings.Builder, p *daggerv1.Project) {
	fmt.Fprintf(b, "%-24s %s", p.GetSlug(), p.GetTitle())
	if p.GetArchivedAt() != nil {
		b.WriteString("  (archived)")
	}
	b.WriteByte('\n')
}

func writeLease(b *strings.Builder, l *daggerv1.Lease) {
	fmt.Fprintf(b, "lease %s", l.GetId())
	if l.GetReleaseReason() != daggerv1.ReleaseReason_RELEASE_REASON_UNSPECIFIED {
		fmt.Fprintf(b, " released: %s\n", enumName(l.GetReleaseReason().String()))
		return
	}
	fmt.Fprintf(b, " expires in %s\n", time.Until(l.GetExpiresAt().AsTime()).Round(time.Second))
}

func writeReason(b *strings.Builder, r *daggerv1.Reason) {
	switch x := r.GetReason().(type) {
	case *daggerv1.Reason_NotOpen:
		fmt.Fprintf(b, "not open: %s\n", enumName(x.NotOpen.GetStatus().String()))
	case *daggerv1.Reason_Blocked:
		gate := ""
		if x.Blocked.GetGate() {
			gate = " (gate)"
		}
		fmt.Fprintf(b, "blocked by #%d %s%s\n", x.Blocked.GetBlocker().GetNumber(), x.Blocked.GetBlocker().GetTitle(), gate)
	case *daggerv1.Reason_Leased:
		fmt.Fprintf(b, "leased by %s until %s\n", x.Leased.GetLease().GetHolder(), x.Leased.GetLease().GetExpiresAt().AsTime().Format(time.RFC3339))
	case *daggerv1.Reason_OpenChild:
		fmt.Fprintf(b, "open child #%d %s\n", x.OpenChild.GetChild().GetNumber(), x.OpenChild.GetChild().GetTitle())
	case *daggerv1.Reason_Archived:
		fmt.Fprintf(b, "project %s is archived\n", x.Archived.GetProject().GetSlug())
	}
}

func writePageToken(b *strings.Builder, token string) {
	if token != "" {
		fmt.Fprintf(b, "next page: --page-token %s\n", token)
	}
}
