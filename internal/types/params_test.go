package types_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// member names a param and its type as an .mro spells it.
type member struct{ Name, Type string }

func members(ps []*syntax.StructMember) []member {
	out := make([]member, len(ps))
	for i, p := range ps {
		out[i] = member{p.Id, p.Tname.String()}
	}

	return out
}

// TestParams pins the role -> parameter-set selection every bundle producer
// relies on: a split stage's chunk inputs, and its outputs with its chunk
// outputs for a chunk's main; a pipeline has no chunk parameters.
func TestParams(t *testing.T) {
	ast := compile(t, typesSrc)
	stage, top := ast.Callables.Table["MAKE"], ast.Callables.Table["TOP"]

	cases := []struct {
		name string
		c    syntax.Callable
		role string
		want []member
	}{
		{"stage in", stage, types.RoleIn, []member{{"scale", "float"}}},
		{"stage out", stage, types.RoleOut, []member{{"data", "file"}, {"count", "int"}}},
		{"stage chunk in", stage, types.RoleChunkIn, []member{{"part", "int"}}},
		{"stage main out", stage, types.RoleMainOut, []member{{"data", "file"}, {"count", "int"}, {"piece", "txt"}}},
		{"pipeline in", top, types.RoleIn, []member{{"k", "float"}}},
		{"pipeline out", top, types.RoleOut, []member{{"made", "MAKE"}}},
		{"pipeline chunk in", top, types.RoleChunkIn, []member{}},
		{"pipeline main out", top, types.RoleMainOut, []member{{"made", "MAKE"}}},
	}

	for _, tc := range cases {
		ps, err := types.Params(tc.c, tc.role)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)

			continue
		}

		if diff := cmp.Diff(tc.want, members(ps)); diff != "" {
			t.Errorf("%s mismatch (-want +got):\n%s", tc.name, diff)
		}
	}

	if _, err := types.Params(stage, "bogus-role"); !errors.Is(err, types.ErrUnknownRole) {
		t.Errorf("Params(MAKE, bogus-role) = %v, want %v", err, types.ErrUnknownRole)
	}
}

// TestCorpusParamsResolve is a robustness sweep over rich, real Martian MRO
// files (structs, typed maps, map calls over arrays and maps, projection,
// wildcards, duck typing, disabled): every parameter of every callable, in
// every role, and every struct member resolves in the type table the file
// compiles to.
func TestCorpusParamsResolve(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "corpus")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".mro" {
			continue
		}

		t.Run(entry.Name(), func(t *testing.T) {
			_, _, ast, err := syntax.Compile(filepath.Join(dir, entry.Name()), []string{dir}, false)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			if len(ast.Callables.List)+len(ast.StructTypes) == 0 {
				t.Fatal("no callables or structs")
			}

			tbl := types.NewTable(&ast.TypeTable)

			for _, c := range ast.Callables.List {
				checkParamsResolve(t, tbl, c)
			}

			for _, st := range ast.StructTypes {
				for _, m := range st.Members {
					if _, err := tbl.Type(m.Tname); err != nil {
						t.Errorf("struct %s member %s: %v", st.Id, m.Id, err)
					}
				}
			}
		})
	}
}

func checkParamsResolve(t *testing.T, tbl *types.Table, c syntax.Callable) {
	t.Helper()

	for _, role := range []string{types.RoleIn, types.RoleOut, types.RoleChunkIn, types.RoleMainOut} {
		ps, err := types.Params(c, role)
		if err != nil {
			t.Fatalf("%s %s: %v", c.GetId(), role, err)
		}

		for _, p := range ps {
			if _, err := tbl.Type(p.Tname); err != nil {
				t.Errorf("%s %s %s: %v", c.GetId(), role, p.Id, err)
			}
		}
	}
}
