package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// marker builds a file transport-marker leaf value as mre writes it into a
// sidecar; dirMarker is its directory counterpart (a staged `path`/dir leaf).
func marker(n string) string    { return shim.FileMarker + "f/" + n }
func dirMarker(n string) string { return shim.DirMarker + "f/" + n }

// mustPublish runs publishOuts and fails the test on a shape error, returning the
// published outs tree.
func mustPublish(t *testing.T, pub *publisher, params []ir.Param, outs map[string]any) map[string]any {
	t.Helper()

	got, err := pub.publishOuts(params, outs)
	if err != nil {
		t.Fatalf("publishOuts: %v", err)
	}

	return got
}

// TestPublishLayoutMapping pins the funnel-free layout mode (#12): from the raw
// marker-bearing sidecar (no files staged), it maps each leaf's transport
// basename to its outs/ rel path(s) and records one manifest entry per leaf, with
// is_dir taken from the leaf's marker (its ground-truth dir-ness stat'd at
// staging time) — WITHOUT copying anything.
func TestPublishLayoutMapping(t *testing.T) {
	params := []ir.Param{
		{Name: "aln", BaseType: "bam", IsFile: true},
		{Name: "shards", BaseType: "txt", IsFile: true, ArrayDim: 1},
		{Name: "workdir", BaseType: "path", IsFile: true},
	}
	outs := map[string]any{
		"aln":     marker("L0000"),
		"shards":  []any{marker("L0001"), marker("L0002")},
		"workdir": dirMarker("L0003"),
	}

	pub := newPublisher(nil)

	published := mustPublish(t, pub, params, outs)

	wantLayout := map[string][]string{
		"L0000": {"aln.bam"},
		"L0001": {"shards/0.txt"},
		"L0002": {"shards/1.txt"},
		"L0003": {"workdir"},
	}
	if diff := cmp.Diff(wantLayout, pub.layout); diff != "" {
		t.Errorf("layout mismatch (-want +got):\n%s", diff)
	}

	wantPublished := map[string]any{
		"aln":     "aln.bam",
		"shards":  []any{"shards/0.txt", "shards/1.txt"},
		"workdir": "workdir",
	}
	if diff := cmp.Diff(wantPublished, published); diff != "" {
		t.Errorf("published tree mismatch (-want +got):\n%s", diff)
	}

	wantManifest := []manifestEntry{
		{Path: "aln.bam", BaseType: "bam", IsDir: false},
		{Path: "shards/0.txt", BaseType: "txt", IsDir: false},
		{Path: "shards/1.txt", BaseType: "txt", IsDir: false},
		{Path: "workdir", BaseType: "path", IsDir: true},
	}
	if diff := cmp.Diff(wantManifest, pub.manifest); diff != "" {
		t.Errorf("manifest mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishManifestIsDirGroundTruth guards #43: is_dir comes from the leaf's
// staged marker, not the declared output type. A directory written into a
// `file`-typed out is catalogued is_dir=true, and a plain file written into a
// `path`-typed out is_dir=false — so a downstream control plane reading the
// manifest GETs the right shape regardless of a producer's declared type.
func TestPublishManifestIsDirGroundTruth(t *testing.T) {
	params := []ir.Param{
		{Name: "dir_in_file", BaseType: "file", IsFile: true},  // declared file, staged a dir
		{Name: "file_in_path", BaseType: "path", IsFile: true}, // declared path, staged a file
	}
	outs := map[string]any{
		"dir_in_file":  dirMarker("L0000"),
		"file_in_path": marker("L0001"),
	}

	pub := newPublisher(nil)
	mustPublish(t, pub, params, outs)

	wantManifest := []manifestEntry{
		{Path: "dir_in_file", BaseType: "file", IsDir: true},
		{Path: "file_in_path", BaseType: "path", IsDir: false},
	}
	if diff := cmp.Diff(wantManifest, pub.manifest); diff != "" {
		t.Errorf("ground-truth is_dir manifest mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishShapeMismatchErrors guards #44: a file-bearing output whose runtime
// value contradicts its declared container type is a hard error, not a silent
// pass-through that would leak a raw @mre:file: marker into pipeline_outs.json and
// skip the layout/manifest. Each case is a well-formed marker at the wrong shape.
func TestPublishShapeMismatchErrors(t *testing.T) {
	cases := []struct {
		name    string
		params  []ir.Param
		structs map[string]*ir.StructType
		outs    map[string]any
	}{
		{
			name:   "array param, scalar marker",
			params: []ir.Param{{Name: "shards", BaseType: "txt", IsFile: true, ArrayDim: 1}},
			outs:   map[string]any{"shards": marker("L0000")},
		},
		{
			name:   "map param, scalar marker",
			params: []ir.Param{{Name: "reports", BaseType: "txt", IsFile: true, MapDim: 1}},
			outs:   map[string]any{"reports": marker("L0000")},
		},
		{
			name:    "struct param, scalar marker",
			params:  []ir.Param{{Name: "cfg", BaseType: "Cfg", IsFile: true}},
			structs: map[string]*ir.StructType{"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "data", BaseType: "file", IsFile: true}}}},
			outs:    map[string]any{"cfg": marker("L0000")},
		},
		{
			name:   "scalar file param, array value",
			params: []ir.Param{{Name: "aln", BaseType: "bam", IsFile: true}},
			outs:   map[string]any{"aln": []any{marker("L0000")}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newPublisher(c.structs).publishOuts(c.params, c.outs)
			if err == nil {
				t.Fatalf("expected a shape-mismatch error, got nil")
			}
			if strings.Contains(fmt.Sprint(err), shim.FileMarker) {
				t.Errorf("error must not embed the raw marker value: %v", err)
			}
		})
	}
}

// TestPublishOutsTreeLayout pins the published outs/ tree to Martian's
// post_process layout: a scalar file is named by GetOutFilename at the root; an
// array becomes a subdir (bare param name) with zero-padded index filenames; a
// typed map a subdir with key filenames; a struct a subdir with each field named
// by its own GetOutFilename. Leaf JSON values are the path within the outs dir.
func TestPublishOutsTreeLayout(t *testing.T) {
	params := []ir.Param{
		{Name: "alignments", BaseType: "bam", IsFile: true},          // scalar user file -> name.bam
		{Name: "shards", BaseType: "csv", IsFile: true, ArrayDim: 1}, // array -> shards/<idx>.csv
		{Name: "reports", BaseType: "txt", IsFile: true, MapDim: 1},  // map -> reports/<key>.txt
		{Name: "cfg", BaseType: "MyStruct", IsFile: true},            // struct -> cfg/<field>
		{Name: "count", BaseType: "int"},                             // non-file scalar passes through
	}
	structs := map[string]*ir.StructType{
		"MyStruct": {Name: "MyStruct", Fields: []ir.Param{{Name: "data", BaseType: "file", IsFile: true}}},
	}
	outs := map[string]any{
		"alignments": marker("L0000"),
		"shards":     []any{marker("L0001"), marker("L0002")},
		"reports":    map[string]any{"sampleA": marker("L0003"), "sampleB": marker("L0004")},
		"cfg":        map[string]any{"data": marker("L0005")},
		"count":      float64(7),
	}

	got := mustPublish(t, newPublisher(structs), params, outs)

	want := map[string]any{
		"alignments": "alignments.bam",
		"shards":     []any{"shards/0.csv", "shards/1.csv"},
		"reports":    map[string]any{"sampleA": "reports/sampleA.txt", "sampleB": "reports/sampleB.txt"},
		"cfg":        map[string]any{"data": "cfg/data"},
		"count":      float64(7),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishLayoutOutNameCollision pins that two distinct leaves colliding on
// one outs/ rel (two outputs sharing an explicit OutName) are disambiguated with
// a numeric suffix — so neither file is silently mapped over the other — in the
// published values, the layout, and the manifest alike.
func TestPublishLayoutOutNameCollision(t *testing.T) {
	params := []ir.Param{
		{Name: "a", BaseType: "file", IsFile: true, OutName: "shared.txt"},
		{Name: "b", BaseType: "file", IsFile: true, OutName: "shared.txt"},
	}
	outs := map[string]any{
		"a": marker("L0000"),
		"b": marker("L0001"),
	}

	pub := newPublisher(nil)

	got := mustPublish(t, pub, params, outs)

	if got["a"] != "shared.txt" || got["b"] != "shared_1.txt" {
		t.Fatalf("collision not disambiguated: a=%v b=%v", got["a"], got["b"])
	}

	want := map[string][]string{"L0000": {"shared.txt"}, "L0001": {"shared_1.txt"}}
	if diff := cmp.Diff(want, pub.layout); diff != "" {
		t.Errorf("collision layout mismatch (-want +got):\n%s", diff)
	}

	// The manifest must record the ACTUAL published path (post-disambiguation), not
	// the pre-collision rel — otherwise it points two entries at 'shared.txt'.
	wantManifest := []manifestEntry{
		{Path: "shared.txt", BaseType: "file", IsDir: false},
		{Path: "shared_1.txt", BaseType: "file", IsDir: false},
	}
	if diff := cmp.Diff(wantManifest, pub.manifest); diff != "" {
		t.Errorf("collision manifest mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishLayoutRepeatedLeafDedup pins that ONE leaf referenced by two
// outputs resolving to the same rel publishes once: the rel appears once in
// layout[base] and once in the manifest — re-establishing, for layout mode, the
// same-source dedup the copying mode had. Without it the PUBLISH_LEAF fan-out
// would spawn two tasks writing the same outs/ destination and the manifest
// would double-count the output.
func TestPublishLayoutRepeatedLeafDedup(t *testing.T) {
	params := []ir.Param{
		{Name: "a", BaseType: "file", IsFile: true, OutName: "shared.txt"},
		{Name: "b", BaseType: "file", IsFile: true, OutName: "shared.txt"},
	}
	outs := map[string]any{"a": marker("L0000"), "b": marker("L0000")}

	pub := newPublisher(nil)

	got := mustPublish(t, pub, params, outs)

	if got["a"] != "shared.txt" || got["b"] != "shared.txt" {
		t.Fatalf("repeated leaf rels = %v/%v, want shared.txt/shared.txt", got["a"], got["b"])
	}

	wantLayout := map[string][]string{"L0000": {"shared.txt"}}
	if diff := cmp.Diff(wantLayout, pub.layout); diff != "" {
		t.Errorf("repeated-leaf layout mismatch (-want +got):\n%s", diff)
	}

	wantManifest := []manifestEntry{{Path: "shared.txt", BaseType: "file", IsDir: false}}
	if diff := cmp.Diff(wantManifest, pub.manifest); diff != "" {
		t.Errorf("repeated-leaf manifest mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishOutsAbsentFileNull guards the null cases: an empty-string leaf and a
// declared-but-never-written file (which keeps its raw, marker-less path in the
// sidecar) both resolve to null, matching Martian.
func TestPublishOutsAbsentFileNull(t *testing.T) {
	params := []ir.Param{
		{Name: "missing", BaseType: "txt", IsFile: true},
		{Name: "empty", BaseType: "txt", IsFile: true},
	}
	outs := map[string]any{
		"missing": "/scratch/files/never-written.txt",
		"empty":   "",
	}

	got := mustPublish(t, newPublisher(nil), params, outs)

	if got["missing"] != nil || got["empty"] != nil {
		t.Errorf("absent/empty leaves = %v/%v, want nil/nil", got["missing"], got["empty"])
	}
}

// TestPublishOutsArrayInStruct covers a struct field that is itself a file array
// (the struct_file_array fixture shape): the leaves nest under <param>/<field>/.
func TestPublishOutsArrayInStruct(t *testing.T) {
	params := []ir.Param{{Name: "r", BaseType: "Report", IsFile: true}}
	structs := map[string]*ir.StructType{"Report": {Name: "Report", Fields: []ir.Param{
		{Name: "files", BaseType: "txt", IsFile: true, ArrayDim: 1},
		{Name: "n", BaseType: "int"},
	}}}
	outs := map[string]any{"r": map[string]any{"files": []any{marker("L0000"), marker("L0001")}, "n": float64(2)}}

	got := mustPublish(t, newPublisher(structs), params, outs)

	want := map[string]any{"r": map[string]any{
		"files": []any{"r/files/0.txt", "r/files/1.txt"},
		"n":     float64(2),
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("array-in-struct publish mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishOutsMapOfFileArray guards the MapDim model in the publisher: a
// map<txt[]> output lowers to {MapDim:2, ArrayDim:0}, so the map's array-typed
// values must be descended as arrays (map<file[]> -> <param>/<key>/<idx>.txt),
// not treated as a second map level (which dropped the inner files and leaked
// absolute source paths).
func TestPublishOutsMapOfFileArray(t *testing.T) {
	params := []ir.Param{{Name: "lanes", BaseType: "txt", IsFile: true, MapDim: 2}}
	outs := map[string]any{"lanes": map[string]any{
		"sampleA": []any{marker("L0000"), marker("L0001")},
		"sampleB": []any{marker("L0002")},
	}}

	got := mustPublish(t, newPublisher(nil), params, outs)

	want := map[string]any{"lanes": map[string]any{
		"sampleA": []any{"lanes/sampleA/0.txt", "lanes/sampleA/1.txt"},
		"sampleB": []any{"lanes/sampleB/0.txt"},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("map<file[]> publish mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishOutsMapIllegalKeyWarns guards #114: a file map output under a key
// that is not a legal Unix filename is dropped from the published tree, the
// layout, AND the manifest — matching mrp, whose post_process writes only legal
// keys back into the outs JSON (martian/core/post_process.go moveOutFiles,
// TypedMapType case) — but the drop must be LOUD: one stderr warning per skipped
// key, naming the key, the output, and the reason, mirroring Martian's printed
// error. Legal keys publish normally and are not warned about.
func TestPublishOutsMapIllegalKeyWarns(t *testing.T) {
	longKey := strings.Repeat("k", 256)
	params := []ir.Param{{Name: "reports", BaseType: "txt", IsFile: true, MapDim: 1}}
	outs := map[string]any{"reports": map[string]any{
		"a/b":   marker("L0000"),
		"":      marker("L0001"),
		"..":    marker("L0002"),
		longKey: marker("L0003"),
		"ok":    marker("L0004"),
	}}

	pub := newPublisher(nil)

	var warnings strings.Builder

	pub.warn = &warnings

	got := mustPublish(t, pub, params, outs)

	want := map[string]any{"reports": map[string]any{"ok": "reports/ok.txt"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}

	wantLayout := map[string][]string{"L0004": {"reports/ok.txt"}}
	if diff := cmp.Diff(wantLayout, pub.layout); diff != "" {
		t.Errorf("layout mismatch (-want +got):\n%s", diff)
	}

	wantManifest := []manifestEntry{{Path: "reports/ok.txt", BaseType: "txt", IsDir: false}}
	if diff := cmp.Diff(wantManifest, pub.manifest); diff != "" {
		t.Errorf("manifest mismatch (-want +got):\n%s", diff)
	}

	for _, k := range []string{"a/b", "", "..", longKey} {
		wantLine := fmt.Sprintf("mre: publish: skipping map key %q of output %q: ", k, "reports")
		if !strings.Contains(warnings.String(), wantLine) {
			t.Errorf("missing warning for key %q: want substring %q in:\n%s", k, wantLine, warnings.String())
		}
	}

	if n := strings.Count(warnings.String(), "skipping map key"); n != 4 {
		t.Errorf("warning count = %d, want 4 (one per skipped key):\n%s", n, warnings.String())
	}

	if strings.Contains(warnings.String(), `"ok"`) {
		t.Errorf("legal key %q must not be warned about:\n%s", "ok", warnings.String())
	}
}

// TestPublishOutsNonFilePassthrough guards that non-file outputs pass through
// verbatim — a map<int> keeps every key (even ones illegal as filenames, which
// only matter when a file must be named) and a non-file struct keeps its value
// unchanged, matching Martian's non-file outs.
func TestPublishOutsNonFilePassthrough(t *testing.T) {
	params := []ir.Param{
		{Name: "counts", BaseType: "int", MapDim: 1},
		{Name: "vals", BaseType: "int", ArrayDim: 1},
	}
	outs := map[string]any{
		"counts": map[string]any{"a/b": float64(1), "": float64(2), "ok": float64(3)},
		"vals":   []any{float64(1), float64(2)},
	}

	got := mustPublish(t, newPublisher(nil), params, outs)

	want := map[string]any{
		"counts": map[string]any{"a/b": float64(1), "": float64(2), "ok": float64(3)},
		"vals":   []any{float64(1), float64(2)},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("non-file passthrough mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishOutsDeepNesting guards the dim arithmetic at depth: a map<txt[][]>
// (MapDim=3) descends map -> array -> array, and a struct whose field is a
// map<txt> nests as <param>/<field>/<key>.txt. Both are reachable shapes not
// covered by the shallower tests.
func TestPublishOutsDeepNesting(t *testing.T) {
	params := []ir.Param{
		{Name: "grid", BaseType: "txt", IsFile: true, MapDim: 3}, // map<txt[][]>
		{Name: "cfg", BaseType: "Cfg", IsFile: true},             // struct{ map<txt> reports }
	}
	structs := map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "reports", BaseType: "txt", IsFile: true, MapDim: 1}}},
	}
	outs := map[string]any{
		"grid": map[string]any{"k": []any{[]any{marker("L0000"), marker("L0001")}, []any{marker("L0002")}}},
		"cfg":  map[string]any{"reports": map[string]any{"r1": marker("L0003"), "r2": marker("L0004")}},
	}

	got := mustPublish(t, newPublisher(structs), params, outs)

	want := map[string]any{
		"grid": map[string]any{"k": []any{
			[]any{"grid/k/0/0.txt", "grid/k/0/1.txt"},
			[]any{"grid/k/1/0.txt"},
		}},
		"cfg": map[string]any{"reports": map[string]any{
			"r1": "cfg/reports/r1.txt",
			"r2": "cfg/reports/r2.txt",
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deep-nesting publish mismatch (-want +got):\n%s", diff)
	}
}
