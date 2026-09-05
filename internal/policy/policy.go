// Package policy evaluates the authorization policy in process. A bundle
// of Rego modules is compiled once, at startup, and every mutation asks it
// whether a principal may take an action on a resource. The Rego itself
// lives under policy/ at the repository root; this package is the seam
// between it and the domain layer.
//
// Evaluation fails closed. A bundle that did not load, a query that
// errors, and a decision that is anything but an explicit allow are all
// denials. See docs/adrs/004-embedded-opa-authorization.md.
package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/v1/rego"

	defaultbundle "github.com/ivy/dagger/policy"
)

// decisionQuery is the rule the bundle must define: an object with `allow`
// and the `reasons` the action was refused for.
const decisionQuery = "data.dagger.authz.decision"

// ErrDenied is what every refusal unwraps to, whatever its cause.
var ErrDenied = errors.New("denied by policy")

// DeniedError reports a refused action: the reasons the policy gave, or the
// evaluation failure that stood in for a decision.
type DeniedError struct {
	Action  string
	Reasons []string
	// Err is the evaluation failure, when the denial was one. Nil when the
	// policy evaluated and said no.
	Err error
}

func (e *DeniedError) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("%s: %s: policy evaluation failed: %v", e.Action, ErrDenied, e.Err)
	case len(e.Reasons) > 0:
		return fmt.Sprintf("%s: %s: %s", e.Action, ErrDenied, strings.Join(e.Reasons, "; "))
	}
	return fmt.Sprintf("%s: %s", e.Action, ErrDenied)
}

// Is makes every denial match [ErrDenied].
func (e *DeniedError) Is(target error) bool { return target == ErrDenied }

func (e *DeniedError) Unwrap() error { return e.Err }

// Principal is the actor, as the transport resolved it.
type Principal struct {
	ID string `json:"id"`
	// Kind is `user`, `agent`, or `service`.
	Kind string `json:"kind"`
}

// Ancestor is one step of a node's lineage.
type Ancestor struct {
	ID string `json:"id"`
	// CreatedBy is the principal whose create_node event made the node.
	// Empty when it has none.
	CreatedBy string `json:"created_by"`
}

// Node is a node as policy sees it.
type Node struct {
	ID           string  `json:"id"`
	ProjectID    string  `json:"project_id"`
	ParentID     *string `json:"parent_id"`
	Kind         string  `json:"kind"`
	Status       string  `json:"status"`
	AssigneeKind *string `json:"assignee_kind"`
	Assignee     *string `json:"assignee"`
	// CreatedBy is the principal whose create_node event made the node.
	CreatedBy string `json:"created_by"`
	// Lineage is the node itself followed by each ancestor up to the root.
	Lineage []Ancestor `json:"lineage"`
}

// Edge is a directed edge: From blocks To.
type Edge struct {
	From Node   `json:"from"`
	To   Node   `json:"to"`
	Kind string `json:"kind"`
}

// Lease is a lease as policy sees it.
type Lease struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id"`
	Holder string `json:"holder"`
}

// Comment is a comment as policy sees it.
type Comment struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id"`
	// Author is the principal that wrote it. Editing does not change it:
	// the schema keeps one author per comment, so only the author edits.
	Author    string  `json:"author"`
	InReplyTo *string `json:"in_reply_to"`
	Resolved  bool    `json:"resolved"`
}

// Usage is a usage record as policy sees it.
type Usage struct {
	NodeID string `json:"node_id"`
	// LeaseID is the attempt an actual is attributed to; nil on an
	// estimate.
	LeaseID *string `json:"lease_id"`
	// Principal is who reported the actual or made the estimate.
	Principal string `json:"principal"`
	// Kind is `actual` or `estimate`.
	Kind string `json:"kind"`
}

// Resource is what an action applies to. One of Node and Edge is set;
// Lease accompanies Node for lease actions, Comment accompanies Node for
// comment actions, and Usage accompanies Node for usage actions.
type Resource struct {
	Node    *Node    `json:"node,omitempty"`
	Edge    *Edge    `json:"edge,omitempty"`
	Lease   *Lease   `json:"lease,omitempty"`
	Comment *Comment `json:"comment,omitempty"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Input is one authorization question. Action is the operation name as
// events record it. Resource is the state before, After the proposed state
// after, and Lease the lease the request carries, if any.
type Input struct {
	Principal Principal `json:"principal"`
	Action    string    `json:"action"`
	Resource  Resource  `json:"resource"`
	After     *Resource `json:"after"`
	Lease     *Lease    `json:"lease"`
}

// Bundle is a compiled policy, safe for concurrent use.
type Bundle struct {
	query   rego.PreparedEvalQuery
	version string
}

// Load compiles every `.rego` module under fsys into a bundle, leaving
// `_test.rego` files to opa test. A bundle with no modules, or one that
// does not compile, is an error and no bundle.
func Load(ctx context.Context, fsys fs.FS) (*Bundle, error) {
	modules, err := readModules(fsys)
	if err != nil {
		return nil, err
	}
	if len(modules) == 0 {
		return nil, errors.New("policy bundle has no modules")
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)

	opts := []func(*rego.Rego){rego.Query(decisionQuery), rego.StrictBuiltinErrors(true)}
	digest := sha256.New()
	for _, name := range names {
		opts = append(opts, rego.Module(name, modules[name]))
		// Name and source, each NUL-terminated, so moving bytes between
		// files or names changes the digest.
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		digest.Write([]byte(modules[name]))
		digest.Write([]byte{0})
	}
	query, err := rego.New(opts...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile policy bundle: %w", err)
	}
	return &Bundle{query: query, version: "sha256:" + hex.EncodeToString(digest.Sum(nil))}, nil
}

// LoadDefault compiles the bundle shipped in the repository.
func LoadDefault(ctx context.Context) (*Bundle, error) {
	return Load(ctx, defaultbundle.FS)
}

// readModules collects the policy modules under fsys by path.
func readModules(fsys fs.FS) (map[string]string, error) {
	modules := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".rego" || strings.HasSuffix(p, "_test.rego") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		modules[p] = string(src)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read policy bundle: %w", err)
	}
	return modules, nil
}

// Version identifies the bundle by the digest of its modules, so every
// event can say which policy authorized it. Empty for a nil bundle.
func (b *Bundle) Version() string {
	if b == nil {
		return ""
	}
	return b.version
}

// Authorize evaluates in and returns nil when the policy allows it, or a
// [*DeniedError] otherwise. A nil bundle denies everything: nothing loaded
// is no policy, and no policy is no permission.
func (b *Bundle) Authorize(ctx context.Context, in Input) error {
	if b == nil {
		return &DeniedError{Action: in.Action, Err: errors.New("no policy bundle is loaded")}
	}
	// Round-tripping through JSON hands the evaluator exactly the document
	// the Rego is written against, whatever Go types built it.
	encoded, err := json.Marshal(in)
	if err != nil {
		return &DeniedError{Action: in.Action, Err: fmt.Errorf("encode input: %w", err)}
	}
	var doc any
	if err := json.Unmarshal(encoded, &doc); err != nil {
		return &DeniedError{Action: in.Action, Err: fmt.Errorf("encode input: %w", err)}
	}

	results, err := b.query.Eval(ctx, rego.EvalInput(doc))
	if err != nil {
		return &DeniedError{Action: in.Action, Err: err}
	}
	decision, err := parseDecision(results)
	if err != nil {
		return &DeniedError{Action: in.Action, Err: err}
	}
	if !decision.allow {
		return &DeniedError{Action: in.Action, Reasons: decision.reasons}
	}
	return nil
}

type decision struct {
	allow   bool
	reasons []string
}

// parseDecision reads the decision object out of a result set. Anything
// other than exactly one object whose allow is the boolean true is not an
// allow.
func parseDecision(results rego.ResultSet) (decision, error) {
	if len(results) != 1 || len(results[0].Expressions) != 1 {
		return decision{}, fmt.Errorf("%s is undefined", decisionQuery)
	}
	obj, ok := results[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return decision{}, fmt.Errorf("%s is not an object", decisionQuery)
	}
	allow, ok := obj["allow"].(bool)
	if !ok {
		return decision{}, fmt.Errorf("%s.allow is not a boolean", decisionQuery)
	}
	d := decision{allow: allow}
	if reasons, ok := obj["reasons"].([]any); ok {
		for _, r := range reasons {
			d.reasons = append(d.reasons, fmt.Sprint(r))
		}
		sort.Strings(d.reasons)
	}
	return d, nil
}
