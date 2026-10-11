package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// leaf is the path below the files dir of the leaf in ordinal dir n. Every
// leaf shares one basename, as the outputs of sibling chunks do, so the layout
// must tell leaves apart by their ordinal.
func leaf(n string) string { return n + "/out" }

// marker builds a file transport-marker leaf value as mre writes it into a
// sidecar; dirMarker is its directory counterpart (a staged `path`/dir leaf).
func marker(n string) string    { return shim.FileMarker + "f/" + leaf(n) }
func dirMarker(n string) string { return shim.DirMarker + "f/" + leaf(n) }

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
// marker-bearing sidecar (no files staged), it maps each leaf's path below the
// files dir to its outs/ rel path(s) and records one manifest entry per leaf, with
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
		leaf("L0000"): {"aln.bam"},
		leaf("L0001"): {"shards/0.txt"},
		leaf("L0002"): {"shards/1.txt"},
		leaf("L0003"): {"workdir"},
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

// TestPublishKeepsMarkerShapedStrings guards #328 at publish: a marker-shaped
// string output reaches pipeline_outs.json as the literal string mrp
// publishes, and a file-typed output holding one names no file, so it
// publishes null and adds nothing to the layout. The sidecar is the one
// WriteBundle produces, as on the real path.
func TestPublishKeepsMarkerShapedStrings(t *testing.T) {
	const shaped = "@mre:file:f/L0000/x.txt"

	params := []ir.Param{
		{Name: "s", BaseType: "string"},
		{Name: "m", BaseType: "map"},
		{Name: "fake", BaseType: "txt", IsFile: true},
	}
	outs := map[string]any{
		"s":     shaped,
		"m":     map[string]any{"k": []any{shaped}},
		"fake":  shaped,
		"extra": shaped,
	}

	dir := filepath.Join(t.TempDir(), "outs")
	if err := shim.WriteBundle(dir, outs, params, types.NewTable(nil), nil); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	sidecar, err := rawToMap(raw)
	if err != nil {
		t.Fatal(err)
	}

	pub := newPublisher(nil)
	got := mustPublish(t, pub, params, sidecar)

	want := map[string]any{
		"s":     shaped,
		"m":     types.Object{{Key: "k", Value: []any{shaped}}},
		"fake":  nil,
		"extra": shaped,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}

	if len(pub.layout) != 0 {
		t.Errorf("layout = %v, want empty: no output names a file", pub.layout)
	}
}

// TestPublishRejectsMalformedMarker checks a file leaf carrying the reserved
// marker prefix in a shape mre never writes fails the publish instead of being
// laid out.
func TestPublishRejectsMalformedMarker(t *testing.T) {
	params := []ir.Param{{Name: "f", BaseType: "txt", IsFile: true}}
	outs := map[string]any{"f": shim.FileMarker + "../elsewhere"}

	if _, err := newPublisher(nil).publishOuts(params, outs); !errors.Is(err, shim.ErrBadMarker) {
		t.Errorf("publishOuts err = %v, want ErrBadMarker", err)
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

	want := map[string][]string{leaf("L0000"): {"shared.txt"}, leaf("L0001"): {"shared_1.txt"}}
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
// layout[leaf] and once in the manifest — re-establishing, for layout mode, the
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

	wantLayout := map[string][]string{leaf("L0000"): {"shared.txt"}}
	if diff := cmp.Diff(wantLayout, pub.layout); diff != "" {
		t.Errorf("repeated-leaf layout mismatch (-want +got):\n%s", diff)
	}

	wantManifest := []manifestEntry{{Path: "shared.txt", BaseType: "file", IsDir: false}}
	if diff := cmp.Diff(wantManifest, pub.manifest); diff != "" {
		t.Errorf("repeated-leaf manifest mismatch (-want +got):\n%s", diff)
	}
}

// TestPublishSharedLeafNamesFirstPath pins #431: a leaf reached more than once
// is named by the first path mrp's walk publishes it at (outputs in
// declaration order, struct fields sorted, arrays by index) at every
// reference, while each later path still gets its copy in the layout and the
// manifest.
func TestPublishSharedLeafNamesFirstPath(t *testing.T) {
	params := []ir.Param{
		{Name: "primary", BaseType: "json", IsFile: true, ArrayDim: 1},
		{Name: "alias", BaseType: "json", IsFile: true, ArrayDim: 1},
		{Name: "pair", BaseType: "Pair", IsFile: true},
		{Name: "d2", BaseType: "path", IsFile: true},
		{Name: "d1", BaseType: "path", IsFile: true},
	}
	structs := map[string]*ir.StructType{"Pair": {Name: "Pair", Fields: []ir.Param{
		{Name: "zeta", BaseType: "txt", IsFile: true},
		{Name: "alpha", BaseType: "txt", IsFile: true},
	}}}
	js := []any{marker("L0000"), nil, marker("L0001"), marker("L0000")}
	outs := map[string]any{
		"primary": js,
		"alias":   js,
		"pair":    map[string]any{"zeta": marker("L0002"), "alpha": marker("L0002")},
		"d2":      dirMarker("L0003"),
		"d1":      dirMarker("L0003"),
	}

	pub := newPublisher(structs)
	got := mustPublish(t, pub, params, outs)

	firsts := []any{"primary/0.json", nil, "primary/2.json", "primary/0.json"}
	want := map[string]any{
		"primary": firsts,
		"alias":   firsts,
		"pair":    map[string]any{"alpha": "pair/alpha.txt", "zeta": "pair/alpha.txt"},
		"d2":      "d2",
		"d1":      "d2",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}

	wantLayout := map[string][]string{
		leaf("L0000"): {"primary/0.json", "primary/3.json", "alias/0.json", "alias/3.json"},
		leaf("L0001"): {"primary/2.json", "alias/2.json"},
		leaf("L0002"): {"pair/alpha.txt", "pair/zeta.txt"},
		leaf("L0003"): {"d2", "d1"},
	}
	if diff := cmp.Diff(wantLayout, pub.layout); diff != "" {
		t.Errorf("layout mismatch (-want +got):\n%s", diff)
	}

	if len(pub.manifest) != 10 {
		t.Errorf("manifest has %d entries, want one per published path (10): %v", len(pub.manifest), pub.manifest)
	}
}

// TestPublishNDArrayLeavesFilesUnpublished pins #432: below an array of two or
// more dimensions, whether an output, a struct field or a typed map's value,
// mrp moves no file, so nothing is published and each file, one the stage
// never wrote included, is null; every other value, an escaped string
// included, stays as the stage wrote it, one
// warning names each such output, and a 1-D array still publishes.
func TestPublishNDArrayLeavesFilesUnpublished(t *testing.T) {
	params := []ir.Param{
		{Name: "r12", BaseType: "S0", IsFile: true, ArrayDim: 2},
		{Name: "grid", BaseType: "txt", IsFile: true, ArrayDim: 2},
		{Name: "lanes", BaseType: "txt", IsFile: true, MapDim: 3},
		{Name: "one", BaseType: "txt", IsFile: true, ArrayDim: 1},
	}
	structs := map[string]*ir.StructType{"S0": {Name: "S0", Fields: []ir.Param{
		{Name: "f0", BaseType: "path", IsFile: true},
		{Name: "s", BaseType: "string"},
	}}}
	escaped := "@mre:str:" + marker("L0009")
	outs := map[string]any{
		"r12":   []any{[]any{types.Object{{Key: "f0", Value: dirMarker("L0000")}, {Key: "s", Value: escaped}}}},
		"grid":  []any{[]any{marker("L0001"), nil, "/work/ab/cdef/main/files/unwritten.txt"}, nil},
		"lanes": types.Object{{Key: "k", Value: []any{[]any{marker("L0002")}}}},
		"one":   []any{marker("L0003")},
	}

	var warnings strings.Builder

	pub := newPublisher(structs)
	pub.warn = &warnings
	got := mustPublish(t, pub, params, outs)

	want := map[string]any{
		"r12":   []any{[]any{types.Object{{Key: "f0", Value: nil}, {Key: "s", Value: marker("L0009")}}}},
		"grid":  []any{[]any{nil, nil, nil}, nil},
		"lanes": map[string]any{"k": []any{[]any{nil}}},
		"one":   []any{"one/0.txt"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(map[string][]string{leaf("L0003"): {"one/0.txt"}}, pub.layout); diff != "" {
		t.Errorf("layout mismatch (-want +got):\n%s", diff)
	}

	for _, rel := range []string{"r12", "grid", "lanes/k"} {
		if !strings.Contains(warnings.String(), fmt.Sprintf("files below %q unpublished", rel)) {
			t.Errorf("no warning for %s in %q", rel, warnings.String())
		}
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
		"a/b":     marker("L0000"),
		"":        marker("L0001"),
		"..":      marker("L0002"),
		longKey:   marker("L0003"),
		"a\x00/b": marker("L0005"),
		"a/\x00b": marker("L0006"),
		"ok":      marker("L0004"),
	}}

	pub := newPublisher(nil)

	var warnings strings.Builder

	pub.warn = &warnings

	got := mustPublish(t, pub, params, outs)

	want := map[string]any{"reports": map[string]any{"ok": "reports/ok.txt"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published outs mismatch (-want +got):\n%s", diff)
	}

	wantLayout := map[string][]string{leaf("L0004"): {"reports/ok.txt"}}
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

	// The reason names the FIRST offending rune left-to-right, matching
	// Martian's IsLegalUnixFilename scan: NUL before '/' reports the null-
	// character reason, '/' before NUL reports the '/' reason.
	for k, reason := range map[string]string{
		"a\x00/b": "null characters are not allowed in filenames",
		"a/\x00b": "'/' is not allowed in filenames",
	} {
		wantLine := fmt.Sprintf("mre: publish: skipping map key %q of output %q: %s\n", k, "reports", reason)
		if !strings.Contains(warnings.String(), wantLine) {
			t.Errorf("missing warning for key %q: want substring %q in:\n%s", k, wantLine, warnings.String())
		}
	}

	if n := strings.Count(warnings.String(), "skipping map key"); n != 6 {
		t.Errorf("warning count = %d, want 6 (one per skipped key):\n%s", n, warnings.String())
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

// TestPublishOutsDeepNesting guards the dim arithmetic at depth: a
// map<txt[]>[] ({ArrayDim:1, MapDim:2}) descends array -> map -> array, and a
// struct whose field is a map<txt> nests as <param>/<field>/<key>.txt. Both
// are reachable shapes not covered by the shallower tests.
func TestPublishOutsDeepNesting(t *testing.T) {
	params := []ir.Param{
		{Name: "grid", BaseType: "txt", IsFile: true, ArrayDim: 1, MapDim: 2}, // map<txt[]>[]
		{Name: "cfg", BaseType: "Cfg", IsFile: true},                          // struct{ map<txt> reports }
	}
	structs := map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "reports", BaseType: "txt", IsFile: true, MapDim: 1}}},
	}
	outs := map[string]any{
		"grid": []any{map[string]any{"k": []any{marker("L0000"), marker("L0001")}}, map[string]any{"j": []any{marker("L0002")}}},
		"cfg":  map[string]any{"reports": map[string]any{"r1": marker("L0003"), "r2": marker("L0004")}},
	}

	got := mustPublish(t, newPublisher(structs), params, outs)

	want := map[string]any{
		"grid": []any{
			map[string]any{"k": []any{"grid/0/k/0.txt", "grid/0/k/1.txt"}},
			map[string]any{"j": []any{"grid/1/j/0.txt"}},
		},
		"cfg": map[string]any{"reports": map[string]any{
			"r1": "cfg/reports/r1.txt",
			"r2": "cfg/reports/r2.txt",
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deep-nesting publish mismatch (-want +got):\n%s", diff)
	}
}

// TestMaterializeOuts checks the head-node publish materialisation: from a dir of
// staged leaves and the computed layout, it builds the outs/ tree (a file leaf
// linked to each of its rel paths, a directory leaf mirrored recursively), so a
// single publishDir on LAYOUT can copy it into place without a per-leaf task. A
// layout key with no staged leaf (an absent output) is skipped, not an error.
func TestMaterializeOuts(t *testing.T) {
	root := t.TempDir()
	leaves := filepath.Join(root, "f")
	outs := filepath.Join(root, "outs")

	writeLeaf(t, filepath.Join(leaves, "L0000", "x.bam"), "aln")
	writeLeaf(t, filepath.Join(leaves, "L0001", "x.txt"), "shared")
	writeLeaf(t, filepath.Join(leaves, "L0002", "work", "part.txt"), "chunk")
	writeLeaf(t, filepath.Join(leaves, "L0003", "x.txt"), "other")

	layout := map[string][]string{
		"L0000/x.bam": {"aln.bam"},
		"L0001/x.txt": {"a.txt", "b.txt"}, // one leaf referenced by two outputs
		"L0002/work":  {"workdir"},        // directory leaf
		"L0003/x.txt": {"c.txt"},          // shares L0001's basename
		"L9999/x.txt": {"absent.txt"},     // no staged leaf -> skipped
	}

	if err := materializeOuts(leaves, outs, layout); err != nil {
		t.Fatalf("materializeOuts: %v", err)
	}

	want := map[string]string{
		"aln.bam":          "aln",
		"a.txt":            "shared",
		"b.txt":            "shared",
		"c.txt":            "other",
		"workdir/part.txt": "chunk",
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(outs, rel))
		if err != nil {
			t.Fatalf("read outs/%s: %v", rel, err)
		}

		if string(got) != content {
			t.Errorf("outs/%s = %q, want %q", rel, got, content)
		}
	}

	if _, err := os.Stat(filepath.Join(outs, "absent.txt")); !os.IsNotExist(err) {
		t.Errorf("absent leaf published: got err %v, want IsNotExist", err)
	}
}

// writeLeaf writes content to path, creating parent dirs, failing the test on
// error.
func writeLeaf(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
