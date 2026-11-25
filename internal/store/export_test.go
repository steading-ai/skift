package store

// ReadyFragments are the readiness conditions, each one subquery over the
// node n. ReadyWhere negates every one of them and DerivedSQL selects
// every one un-negated, which is what the structural spec asserts.
var ReadyFragments = map[string]string{
	"unsatisfied blockers": unsatisfiedBlockers,
	"live lease":           liveLease,
	"open children":        openChildren,
	"archived project":     archivedProject,
	"held ancestors":       heldAncestors,
	"held project":         heldProject,
}

// ReadyWhere and DerivedSQL are the two statements every fragment must
// appear in, for the spec that checks they cannot drift apart.
const (
	ReadyWhere = readyWhere
	DerivedSQL = derivedSQL
)
