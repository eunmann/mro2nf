package shim_test

import (
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
