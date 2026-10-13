package shim_test

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// typesSrc declares the filetypes and the struct the bundle tests' params
// are typed by.
const typesSrc = `
filetype txt;
filetype fastq.gz;

struct Cfg(
    file ref,
)
`

// fileTable is the walk over typesSrc's compiled types.
func fileTable(t *testing.T) *types.Table {
	t.Helper()

	_, _, ast, err := syntax.ParseSourceBytes([]byte(typesSrc), "types.mro", nil, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	return types.NewTable(&ast.TypeTable)
}

// fields declares each "name type" pair, the type as an .mro spells it, as a
// param of a bundle's payload.
func fields(t *testing.T, decls ...string) []*syntax.StructMember {
	t.Helper()

	out := make([]*syntax.StructMember, len(decls))

	for i, d := range decls {
		name, tname, _ := strings.Cut(d, " ")

		var id syntax.TypeId
		if err := id.UnmarshalText([]byte(tname)); err != nil {
			t.Fatalf("type of %s: %v", d, err)
		}

		out[i] = &syntax.StructMember{Id: name, Tname: id}
	}

	return out
}
