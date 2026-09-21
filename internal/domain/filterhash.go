package domain

import "sort"

// claimScope is the part of a filter that defines a round-robin cursor's
// identity: two requests with the same scope, however expressed, take
// turns over the same rotation. It is hashed with [requestHash], the same
// canonical-JSON-then-SHA-256 idempotency keys use, so it inherits the
// same stability guarantees.
//
// Projects must already be resolved to ids, not raw refs: a project named
// by slug, by `org/slug`, or by id must hash the same. The slice fields
// are sorted ascending before hashing so that filters differing only in
// argument order still share a cursor.
type claimScope struct {
	Projects     []string `json:"projects,omitempty"`
	Kinds        []string `json:"kinds,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	AnyLabels    []string `json:"any_labels,omitempty"`
	AssigneeKind string   `json:"assignee_kind,omitempty"`
	MinPriority  string   `json:"min_priority,omitempty"`
	Parent       string   `json:"parent,omitempty"`
}

// filterHash returns the [claim_cursors] key for a query whose Projects is
// already resolved to project ids and parentID to a parent id.
func filterHash(q ReadyQuery, parentID string) (string, error) {
	scope := claimScope{
		Projects:     sortedCopy(q.Projects),
		Kinds:        sortedCopy(q.Kinds),
		Labels:       sortedCopy(q.Labels),
		AnyLabels:    sortedCopy(q.AnyLabels),
		AssigneeKind: q.AssigneeKind,
		MinPriority:  q.MinPriority,
		Parent:       parentID,
	}
	return requestHash(scope)
}

func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}
