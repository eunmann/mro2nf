package emit

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// TestFileFlattenNestedClosureVars guards bug 2: nested array/map file
// containers must not reuse one closure parameter name. Emitting `__e` at every
// depth produces `.collect { __e -> ... .collect { __e -> ... } }`, which Groovy
// rejects ("variable __e already declared"), so main.nf fails to compile. The
// closure var is depth-threaded (__e0, __e1, ...); struct fields stay at the
// same depth because they don't open a new closure.
func TestFileFlattenNestedClosureVars(t *testing.T) {
	cases := []struct {
		name string
		p    ir.Param
		want string
	}{
		{
			name: "scalar file array (single dim) uses __e0",
			p:    ir.Param{Name: "reads", BaseType: "txt", IsFile: true, ArrayDim: 1},
			want: "(params.reads ?: []).collect { __e0 -> (__e0 != null ? [file(__e0, glob: false)] : []) }.flatten()",
		},
		{
			name: "nested file array (file[][]) threads __e0/__e1",
			p:    ir.Param{Name: "reads", BaseType: "txt", IsFile: true, ArrayDim: 2},
			want: "(params.reads ?: []).collect { __e0 -> (__e0 ?: []).collect { __e1 -> " +
				"(__e1 != null ? [file(__e1, glob: false)] : []) }.flatten() }.flatten()",
		},
		{
			name: "array of map of file threads distinct vars",
			p:    ir.Param{Name: "reads", BaseType: "txt", IsFile: true, ArrayDim: 1, MapDim: 1},
			want: "(params.reads ?: []).collect { __e0 -> (__e0 ?: [:]).sort { __a, __b -> Mro2nf.compareUtf8(__a.key, __b.key) }.collect { __e1 -> " +
				"(__e1.value != null ? [file(__e1.value, glob: false)] : []) }.flatten() }.flatten()",
		},
		{
			// map<file[]> lowers to {MapDim:2, ArrayDim:0}: one map level whose
			// value is an array of files. The inner dim must be walked as an ARRAY,
			// not as a second map level.
			name: "map of file array descends inner array (MapDim=2)",
			p:    ir.Param{Name: "m", BaseType: "txt", IsFile: true, MapDim: 2},
			want: "(params.m ?: [:]).sort { __a, __b -> Mro2nf.compareUtf8(__a.key, __b.key) }.collect { __e0 -> (__e0.value ?: []).collect { __e1 -> " +
				"(__e1 != null ? [file(__e1, glob: false)] : []) }.flatten() }.flatten()",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fileFlattenExpr("params."+tc.p.Name, tc.p, nil)
			if got != tc.want {
				t.Errorf("fileFlattenExpr =\n  %s\nwant\n  %s", got, tc.want)
			}
			// A reused name across nesting depths is the actual Groovy error.
			if strings.Count(got, "{ __e ->") > 0 {
				t.Errorf("undepth-threaded closure var __e present: %s", got)
			}
		})
	}
}

// TestFileFlattenNoGlob checks every file() the flatten emits over an entry
// value disables globbing (#276): a name like `s[1].txt` must stage that exact
// file, not whatever the glob matches.
func TestFileFlattenNoGlob(t *testing.T) {
	structs := map[string]*ir.StructType{
		"Cfg": {Fields: []ir.Param{
			{Name: "ref", BaseType: "txt", IsFile: true},
			{Name: "refs", BaseType: "txt", IsFile: true, ArrayDim: 1},
			{Name: "n", BaseType: "int"},
		}},
	}

	cases := []ir.Param{
		{Name: "f", BaseType: "txt", IsFile: true},
		{Name: "fa", BaseType: "txt", IsFile: true, ArrayDim: 2},
		{Name: "fm", BaseType: "txt", IsFile: true, MapDim: 2},
		{Name: "cfg", BaseType: "Cfg"},
		{Name: "cfgs", BaseType: "Cfg", ArrayDim: 1},
	}

	for _, p := range cases {
		t.Run(p.Name, func(t *testing.T) {
			got := fileFlattenExpr("params."+p.Name, p, structs)

			files := strings.Count(got, "[file(")
			if files == 0 {
				t.Fatalf("fileFlattenExpr emitted no file(): %s", got)
			}

			if noGlob := strings.Count(got, ", glob: false)]"); noGlob != files {
				t.Errorf("%d of %d file() calls disable globbing: %s", noGlob, files, got)
			}
		})
	}
}
