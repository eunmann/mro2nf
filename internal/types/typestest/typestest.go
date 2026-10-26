// Package typestest declares Martian params for the tests of the packages
// that walk values by their compiled types.
package typestest

import (
	"strings"
	"testing"

	"github.com/martian-lang/martian/martian/syntax"
)

// Fields declares each "name type" pair, the type as an .mro spells it, as
// a struct member: a param, or a member of an object a walk descends.
func Fields(tb testing.TB, decls ...string) []*syntax.StructMember {
	tb.Helper()

	out := make([]*syntax.StructMember, len(decls))

	for i, d := range decls {
		name, tname, _ := strings.Cut(d, " ")

		var id syntax.TypeId
		if err := id.UnmarshalText([]byte(tname)); err != nil {
			tb.Fatalf("type of %s: %v", d, err)
		}

		out[i] = &syntax.StructMember{Id: name, Tname: id}
	}

	return out
}
