package store

import (
	"reflect"
	"strings"
)

// selectList names T's columns, as its db tags spell them, each qualified
// by alias: the select list of a hand-written query whose rows are scanned
// into T by name. A scan by name fails on a column T lacks, so selecting
// alias.* would break this binary on a newer schema that only added a
// column, which the compatibility floor promises it can serve
// (docs/adrs/018-unattended-upgrades.md). sqlc expands * in the generated
// queries for the same reason.
func selectList[T any](alias string) string {
	t := reflect.TypeFor[T]()
	columns := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		if name := t.Field(i).Tag.Get("db"); name != "" && name != "-" {
			columns = append(columns, alias+"."+name)
		}
	}
	return strings.Join(columns, ", ")
}

// nodeColumns is the select list of a [Node] from nodes aliased n.
var nodeColumns = selectList[Node]("n")
