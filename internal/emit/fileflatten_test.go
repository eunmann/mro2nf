package emit

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// checkedLeaf is the flatten's rendering of one non-null file leaf x of entry
// input `input` on the local target.
func checkedLeaf(input, x string) string {
	return "[file(Mro2nf.entryFile('" + input + "', " + x + ", 'local', params.mro2nf_allow_remote_inputs), glob: false)]"
}

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
			want: "(params.reads ?: []).collect { __e0 -> (__e0 != null ? " + checkedLeaf("reads", "__e0") + " : []) }.flatten()",
		},
		{
			name: "nested file array (file[][]) threads __e0/__e1",
			p:    ir.Param{Name: "reads", BaseType: "txt", IsFile: true, ArrayDim: 2},
			want: "(params.reads ?: []).collect { __e0 -> (__e0 ?: []).collect { __e1 -> " +
				"(__e1 != null ? " + checkedLeaf("reads", "__e1") + " : []) }.flatten() }.flatten()",
		},
		{
			name: "array of map of file threads distinct vars",
			p:    ir.Param{Name: "reads", BaseType: "txt", IsFile: true, ArrayDim: 1, MapDim: 1},
			want: "(params.reads ?: []).collect { __e0 -> (__e0 ?: [:]).sort { __a, __b -> Mro2nf.compareUtf8(__a.key, __b.key) }.collect { __e1 -> " +
				"(__e1.value != null ? " + checkedLeaf("reads", "__e1.value") + " : []) }.flatten() }.flatten()",
		},
		{
			// map<file[]> lowers to {MapDim:2, ArrayDim:0}: one map level whose
			// value is an array of files. The inner dim must be walked as an ARRAY,
			// not as a second map level.
			name: "map of file array descends inner array (MapDim=2)",
			p:    ir.Param{Name: "m", BaseType: "txt", IsFile: true, MapDim: 2},
			want: "(params.m ?: [:]).sort { __a, __b -> Mro2nf.compareUtf8(__a.key, __b.key) }.collect { __e0 -> (__e0.value ?: []).collect { __e1 -> " +
				"(__e1 != null ? " + checkedLeaf("m", "__e1") + " : []) }.flatten() }.flatten()",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fileFlattenExpr("params."+tc.p.Name, tc.p, nil, TargetLocal)
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

// flattenShapes are entry inputs of every file-bearing shape, with the struct
// table they need.
func flattenShapes() ([]ir.Param, map[string]*ir.StructType) {
	structs := map[string]*ir.StructType{
		"Cfg": {Fields: []ir.Param{
			{Name: "ref", BaseType: "txt", IsFile: true},
			{Name: "refs", BaseType: "txt", IsFile: true, ArrayDim: 1},
			{Name: "n", BaseType: "int"},
		}},
	}

	return []ir.Param{
		{Name: "f", BaseType: "txt", IsFile: true},
		{Name: "fa", BaseType: "txt", IsFile: true, ArrayDim: 2},
		{Name: "fm", BaseType: "txt", IsFile: true, MapDim: 2},
		{Name: "cfg", BaseType: "Cfg"},
		{Name: "cfgs", BaseType: "Cfg", ArrayDim: 1},
	}, structs
}

// TestFileFlattenNoGlob checks every file() the flatten emits over an entry
// value disables globbing (#276): a name like `s[1].txt` must stage that exact
// file, not whatever the glob matches.
func TestFileFlattenNoGlob(t *testing.T) {
	shapes, structs := flattenShapes()

	for _, p := range shapes {
		t.Run(p.Name, func(t *testing.T) {
			got := fileFlattenExpr("params."+p.Name, p, structs, TargetLocal)

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

// TestFileFlattenChecksEveryLeaf checks every file() the flatten emits takes its
// value through Mro2nf.entryFile with the input's own name and the target
// (#330), so no leaf of any shape reaches file() with an unvetted scheme.
func TestFileFlattenChecksEveryLeaf(t *testing.T) {
	shapes, structs := flattenShapes()

	for _, target := range []Target{TargetLocal, TargetAWSBatch, TargetHealthOmics} {
		for _, p := range shapes {
			t.Run(string(target)+"/"+p.Name, func(t *testing.T) {
				got := fileFlattenExpr("params."+p.Name, p, structs, target)

				files := strings.Count(got, "[file(")
				checked := strings.Count(got, "[file(Mro2nf.entryFile('"+p.Name+"', ")
				policy := strings.Count(got, ", '"+string(target)+"', params.mro2nf_allow_remote_inputs), glob: false)]")

				if files == 0 || checked != files || policy != files {
					t.Errorf("%d file() calls, %d vetted for input %q, %d with target %q: %s",
						files, checked, p.Name, policy, target, got)
				}
			})
		}
	}
}
