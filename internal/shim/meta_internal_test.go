package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// TestWriteSkeletonOutsPrepopulatesFilePaths guards bug 6: the _outs skeleton
// must pre-fill each declared output the way Martian's makeOutArg does, not set
// everything to null. Stages that write to (or assert on) a pre-populated output
// path otherwise fail (e.g. FILTER_BARCODES asserts outs.filtered_metrics_groups
// is not None). Rules (core/stage.go makeOutArg + syntax GetOutFilename):
//   - array dim   -> []
//   - map dim     -> {}
//   - scalar file -> <files>/<filename>, filename = OutName, else bare name for
//     builtin file/path, else name.<typename>
//   - struct / plain scalar -> null
func TestWriteSkeletonOutsPrepopulatesFilePaths(t *testing.T) {
	meta := t.TempDir()
	files := "/work/files"

	params := []ir.Param{
		{Name: "aligned", BaseType: "bam", IsFile: true},                       // user file type -> name.bam
		{Name: "raw", BaseType: "file", IsFile: true},                          // builtin file   -> bare name
		{Name: "outdir", BaseType: "path", IsFile: true},                       // builtin path   -> bare name
		{Name: "count", BaseType: "int"},                                       // plain scalar   -> null
		{Name: "shards", BaseType: "bam", IsFile: true, ArrayDim: 1},           // array          -> []
		{Name: "bykey", BaseType: "bam", IsFile: true, MapDim: 1},              // map            -> {}
		{Name: "cfg", BaseType: "Cfg", IsFile: true},                           // struct         -> null
		{Name: "named", BaseType: "csv", IsFile: true, OutName: "metrics.csv"}, // explicit OutName
	}
	tbl := types.NewTable(map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "ref", BaseType: "file", IsFile: true}}},
	})

	if err := writeSkeletonOuts(meta, files, params, tbl); err != nil {
		t.Fatalf("writeSkeletonOuts: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(meta, "_outs"))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"aligned": filepath.Join(files, "aligned.bam"),
		"raw":     filepath.Join(files, "raw"),
		"outdir":  filepath.Join(files, "outdir"),
		"count":   nil,
		"shards":  []any{},
		"bykey":   map[string]any{},
		"cfg":     nil,
		"named":   filepath.Join(files, "metrics.csv"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("_outs skeleton mismatch (-want +got):\n%s", diff)
	}
}
