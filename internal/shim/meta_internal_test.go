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

// TestResolveResourcesAbsNegativeSentinel guards the adaptive-resource contract:
// Martian treats a negative request as "at least |x|" and its cluster path
// resolves it to the positive |x| before reporting it. The resolved allocation
// the shim writes into _jobinfo/__keys (and the stage reads via
// get_memory_allocation) must therefore be positive, never the raw negative.
func TestResolveResourcesAbsNegativeSentinel(t *testing.T) {
	eff := resolveResources(Resources{}, Resources{MemGB: -4, Threads: -2})

	if eff.MemGB != 4 {
		t.Errorf("mem_gb -4 -> %v, want 4", eff.MemGB)
	}
	if eff.Threads != 2 {
		t.Errorf("threads -2 -> %v, want 2", eff.Threads)
	}
	if eff.VMemGB != 4+extraVMemGB {
		t.Errorf("vmem_gb -> %v, want %v (|mem| + headroom)", eff.VMemGB, 4+extraVMemGB)
	}
}

// TestResourceNumMatchesMrp pins the __* tokens to mrp's JobResources.ToLazyMap
// formatting: real mrp writes frac_mem SMALL's 0.30078125 GB as 0.301 and its
// vmem 3.30078125 as 3.3 (#243).
func TestResourceNumMatchesMrp(t *testing.T) {
	for in, want := range map[float64]string{
		308.0 / 1024:          "0.301",
		(308.0 + 3072) / 1024: "3.3",
		1.5:                   "1.5",
		2:                     "2",
		0.5:                   "0.5",
		999.4:                 "999",
		1500:                  "1500",
		1e6:                   "1000000",
	} {
		if got := string(resourceNum(in)); got != want {
			t.Errorf("resourceNum(%v) = %s, want %s", in, got, want)
		}
	}
}

// TestJoinOverrideReachesArgs: a split-returned join __mem_gb of 0.5 reaches
// the join's _args unrounded over the stage's 1.5, as in real mrp (#243).
func TestJoinOverrideReachesArgs(t *testing.T) {
	raw, err := withResources(json.RawMessage(`{"x":1}`), Resources{MemGB: 0.5}, Resources{Threads: 1, MemGB: 1.5})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if string(got["__mem_gb"]) != "0.5" || string(got["__vmem_gb"]) != "3.5" {
		t.Errorf("join __mem_gb/__vmem_gb = %s/%s, want 0.5/3.5", got["__mem_gb"], got["__vmem_gb"])
	}
}

// TestMonitorLimitsFollowRetryAllocation: the first attempt enforces the
// resolved request mrp would; a retry enforces the larger memory Nextflow
// escalated the task to, vmem keeping its headroom above it (#243).
func TestMonitorLimitsFollowRetryAllocation(t *testing.T) {
	eff := Resources{Threads: 1, MemGB: 0.5, VMemGB: 3.5}

	tests := []struct {
		name string
		a    Adapter
		want Resources
	}{
		{"first attempt", Adapter{Attempt: 1, AllocBytes: 1 << 30}, eff},
		{"retry", Adapter{Attempt: 2, AllocBytes: 2 << 30}, Resources{Threads: 1, MemGB: 2, VMemGB: 5}},
		{"retry not above request", Adapter{Attempt: 2, AllocBytes: 1 << 28}, eff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, monitorLimits(eff, tt.a)); diff != "" {
				t.Errorf("monitorLimits mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

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
