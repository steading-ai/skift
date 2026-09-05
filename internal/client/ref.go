package client

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// NodeRef is a parsed node reference: "#7" within a project, "slug#7" in a
// project of the caller's organization, or "org/slug#7" fully qualified.
// See the Keys section of docs/domain-model.md. The server resolves the
// reference; the client parses it only to reject a malformed one before a
// round trip and to learn which project the caller means.
type NodeRef struct {
	Org     string
	Project string
	Number  int64
}

// ErrBadNodeRef is the cause of every reference parse failure.
var ErrBadNodeRef = errors.New("expected a node reference like #7, slug#7, or org/slug#7")

// ParseNodeRef parses the three reference forms. A bare number is accepted
// as "#number", for shells that eat the hash.
func ParseNodeRef(s string) (NodeRef, error) {
	hash := strings.IndexByte(s, '#')
	if hash < 0 {
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			hash, s = 0, "#"+s
		} else {
			return NodeRef{}, fmt.Errorf("%w: %q", ErrBadNodeRef, s)
		}
	}
	qualifier, digits := s[:hash], s[hash+1:]
	number, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || number < 1 || strings.ContainsAny(s, " \t\r\n") {
		return NodeRef{}, fmt.Errorf("%w: %q", ErrBadNodeRef, s)
	}
	ref := NodeRef{Number: number}
	if slash := strings.IndexByte(qualifier, '/'); slash >= 0 {
		ref.Org, ref.Project = qualifier[:slash], qualifier[slash+1:]
		if ref.Org == "" || ref.Project == "" || strings.Contains(ref.Project, "/") {
			return NodeRef{}, fmt.Errorf("%w: %q", ErrBadNodeRef, s)
		}
	} else {
		ref.Project = qualifier
	}
	return ref, nil
}

// ProjectRef is the project half of the reference in the form a request's
// project field accepts: "slug" or "org/slug". Empty for a bare "#7".
func (r NodeRef) ProjectRef() string {
	if r.Org != "" {
		return r.Org + "/" + r.Project
	}
	return r.Project
}

// String renders the reference in its canonical form.
func (r NodeRef) String() string {
	return fmt.Sprintf("%s#%d", r.ProjectRef(), r.Number)
}
