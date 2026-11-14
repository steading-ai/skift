package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/client"
	"github.com/steading-ai/skift/internal/codec"
)

func parseRef(s string) (client.NodeRef, error) {
	ref, err := client.ParseNodeRef(s)
	if err != nil {
		return client.NodeRef{}, &usageError{err: err}
	}
	return ref, nil
}

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

func enumChoices[E protoEnum]() []string {
	var zero E
	return codec.EnumNames(zero.Descriptor())
}

func shortNames[E protoreflect.Enum](values ...E) []string {
	names := make([]string, 0, len(values))
	for _, v := range values {
		names = append(names, codec.EnumName(v))
	}
	return names
}

type protoEnum interface {
	~int32
	Descriptor() protoreflect.EnumDescriptor
}

func unwrap[T any](res *connect.Response[T], err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func orMint(id string) string {
	if id == "" {
		return client.NewRequestID()
	}
	return id
}

func parseView(value string, def skiftv1.View) (skiftv1.View, error) {
	v, err := parseEnum[skiftv1.View]("view", value)
	if err != nil {
		return 0, err
	}
	if v == skiftv1.View_VIEW_UNSPECIFIED {
		return def, nil
	}
	return v, nil
}

func viewName(v skiftv1.View) string {
	return codec.EnumName(v)
}

func viewProp(def skiftv1.View) map[string]any {
	return enumProp(strings.ToUpper(viewHelp[:1])+viewHelp[1:]+". Defaults to "+viewName(def)+".", enumChoices[skiftv1.View]())
}

func splitView(raw json.RawMessage, def skiftv1.View) (skiftv1.View, json.RawMessage, error) {
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
	view, err := parseView(value, def)
	if err != nil {
		return 0, nil, err
	}
	rest, err := json.Marshal(args)
	if err != nil {
		return 0, nil, usage("arguments: %v", err)
	}
	return view, rest, nil
}

const viewHelp = `how much of each node to return: refs (reference, status, and flags), summary (plus title, kind, priority, labels), or full (everything, body included)`

func holdOp(node, reason string, release bool) *skiftv1.PatchOp {
	if release {
		return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_UnholdNode{
			UnholdNode: &skiftv1.UnholdNodeOp{Node: node, Reason: reason},
		}}
	}
	return &skiftv1.PatchOp{Op: &skiftv1.PatchOp_HoldNode{
		HoldNode: &skiftv1.HoldNodeOp{Node: node, Reason: reason},
	}}
}

func claimNext(ctx context.Context, c *Execution, req *skiftv1.ClaimNextRequest, wait time.Duration) (*skiftv1.ClaimNextResponse, error) {
	var draining <-chan struct{}
	if c.Draining != nil {
		draining = c.Draining(ctx)
	}
	var deadline time.Time
	if wait >= 0 {
		deadline = time.Now().Add(wait)
	}
	for {
		select {
		case <-draining:
			return &skiftv1.ClaimNextResponse{}, nil
		default:
		}
		chunk := claimChunk
		if wait >= 0 {
			chunk = min(chunk, time.Until(deadline))
		}
		req.Wait = durationpb.New(max(chunk, 0))
		res, err := c.Work.ClaimNext(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		if res.Msg.GetNode() != nil || wait == 0 || (wait > 0 && !time.Now().Before(deadline)) {
			return res.Msg, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for work: %w", err)
		}
		select {
		case <-draining:
			return res.Msg, nil
		default:
		}
	}
}

const claimChunk = time.Minute

func (a *Execution) project() (string, error) {
	project, err := a.currentProject()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", usage("a project is required; pass a project argument or configure an execution default")
	}
	return project, nil
}

func (a *Execution) currentProject() (string, error) {
	projects := a.currentProjects()
	switch len(projects) {
	case 0:
		return "", nil
	case 1:
		return projects[0], nil
	default:
		return "", usage("only one project default is allowed here; got %d", len(projects))
	}
}

func (a *Execution) projectFor(refs ...client.NodeRef) (string, error) {
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
	return "", usage("a bare #number reference needs a project; qualify the reference as slug#number or configure an execution default")
}

func (a *Execution) nodeArg(arg string) (project, ref string, err error) {
	parsed, err := parseRef(arg)
	if err != nil {
		return "", "", err
	}
	current, err := a.currentProject()
	if err != nil {
		return "", "", err
	}
	if parsed.ProjectRef() == "" && current == "" {
		return "", "", usage("a bare #number reference needs a project; qualify the reference as slug#number or configure an execution default")
	}
	return current, parsed.String(), nil
}
