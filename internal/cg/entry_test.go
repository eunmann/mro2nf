package cg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// entryFanMRO maps USE over an entry file array, beside a broadcast
// reference file, and runs SUM on USE's outs and a relative file value.
const entryFanMRO = `filetype txt;

stage USE(
    in  txt f,
    in  txt ref,
    out int n,
    src py  "stages/use",
)

stage SUM(
    in  int[] ns,
    in  txt   rel,
    out int   total,
    src py    "stages/sum",
)

pipeline FAN(
    in  txt[] fs,
    in  txt   ref,
    in  txt   rel,
    out int   total,
)
{
    map call USE(
        f   = split self.fs,
        ref = self.ref,
    )

    call SUM(
        ns  = USE.n,
        rel = self.rel,
    )

    return (
        total = SUM.total,
    )
}

call FAN(
    fs  = ["/data/a.txt", "s3://bucket/b.txt", "/data/a.txt"],
    ref = "/ref/genome.txt",
    rel = "input/rel.txt",
)
`

func planOf(t *testing.T, mro string) Plan {
	t.Helper()

	return planUnder(t, mro, "")
}

// planUnder plans the invocation mro holds with its s3:// entry file values
// under mount, as mre plan plans a run.
func planUnder(t *testing.T, mro string, mount shim.Mount) Plan {
	t.Helper()

	src := sourceOf(t, mro)

	inv, err := MountEntryValues(src, src.Invocation, nil, mount)
	if err != nil {
		t.Fatal(err)
	}

	p, err := MakePlan(src, inv, nil, t.TempDir(), mount)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func sourceOf(t *testing.T, mro string) Source {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(path, []byte(mro), 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := ReadSource(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	return src
}

// TestPlanStagesEachForksOwnEntryFiles: the run's entry file values a task
// stages are its absolute paths and URIs, each once, in type-walk order,
// and a relative path reaches the stage as given; each USE fork stages its
// own element of the mapped array and the broadcast reference, never the
// other forks' files, and SUM, whose one file value is relative, stages
// none.
func TestPlanStagesEachForksOwnEntryFiles(t *testing.T) {
	p := planOf(t, entryFanMRO)

	wantLeaves := []EntryLeaf{
		{Input: "fs", Value: "/data/a.txt"},
		{Input: "fs", Value: "s3://bucket/b.txt"},
		{Input: "ref", Value: "/ref/genome.txt"},
	}
	if d := cmp.Diff(wantLeaves, p.Leaves); d != "" {
		t.Errorf("entry leaves (-want +plan):\n%s", d)
	}

	files := map[string][]PlanFiles{}
	for _, n := range p.Nodes {
		files[n.Fqid] = n.Files
	}

	want := map[string][]PlanFiles{
		"FAN.USE": {{Key: "0", Leaves: []int{0, 2}}, {Key: "1", Leaves: []int{1, 2}}, {Key: "2", Leaves: []int{0, 2}}},
		"FAN.SUM": nil,
	}
	if d := cmp.Diff(want, files); d != "" {
		t.Errorf("entry files per node and fork (-want +plan):\n%s", d)
	}
}

// TestEntryFileReaders: of the witness's nodes, those whose bindings read
// an entry file value read entry files, whatever value the run gives it;
// a stage whose outs are read is no reader for that.
func TestEntryFileReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(path, []byte(entryFanMRO), 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := ReadSource(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := src.Witness(nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := w.EntryFileReaders(w.Manifest())
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(map[string]bool{"FAN.USE": true, "FAN.SUM": true}, got); d != "" {
		t.Errorf("entry file readers (-want +got):\n%s", d)
	}
}

// TestVetEntryLeaf: an s3:// URI is staged on every profile, a head-node
// path only where the profile sees the head node's files, a URI of any
// other scheme only when remote inputs are allowed, and a path Nextflow's
// stage-in script would expand never.
func TestVetEntryLeaf(t *testing.T) {
	cases := []struct {
		value        string
		host, remote bool
		ok           bool
	}{
		{"s3://b/x.txt", false, false, true},
		{"/data/x.txt", true, false, true},
		{"file:///data/x.txt", true, false, true},
		{"/data/x.txt", false, true, false},
		{"https://h/x.txt", true, false, false},
		{"https://h/x.txt", true, true, true},
		{"HTTP://h/x.txt", false, true, true},
		{"/data/s[1].txt", true, true, false},
		{"s3://b/t{a,b}.txt", true, true, false},
		{"/data/$HOME.txt", true, true, false},
		{"/data/a\tb.txt", true, true, false},
	}

	for _, tc := range cases {
		err := VetEntryLeaf(EntryLeaf{Input: "reads", Value: tc.value}, tc.host, tc.remote)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrEntryFile)) {
			t.Errorf("vet %q (host %t, remote %t): %v, want ok %t", tc.value, tc.host, tc.remote, err, tc.ok)
		}
	}
}

// TestStagedEntryFiles: the i-th staged entry file is read where
// Nextflow's link points, the value's own path, as mrp passes it, or where
// it was fetched to; a dir holding no one item fails.
func TestStagedEntryFiles(t *testing.T) {
	p := planOf(t, entryFanMRO)

	path := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(path, []byte(entryFanMRO), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := Load(path, p.Call, nil)
	if err != nil {
		t.Fatal(err)
	}

	task := t.TempDir()
	t.Chdir(task)

	for _, d := range []string{"entry/1", "entry/2", "entry/3"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink("/ref/genome.txt", "entry/1/genome.txt"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile("entry/2/b.txt", nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := g.StagedEntryFiles(g.Manifest(), "entry", []int{2, 1})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"/ref/genome.txt": "/ref/genome.txt", "s3://bucket/b.txt": filepath.Join(task, "entry", "2", "b.txt")}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("staged entry files (-want +got):\n%s", d)
	}

	if _, err := g.StagedEntryFiles(g.Manifest(), "entry", []int{0, 1, 2}); !errors.Is(err, errStagedEntry) {
		t.Errorf("an empty staged dir: %v, want errStagedEntry", err)
	}

	if _, err := g.StagedEntryFiles(g.Manifest(), "entry", []int{7}); !errors.Is(err, errStagedEntry) {
		t.Errorf("an index past the run's values: %v, want errStagedEntry", err)
	}
}

// echoMRO's pipeline outs return its entry file value, which no stage
// reads.
const echoMRO = `filetype txt;

pipeline ECHO(
    in  txt ref,
    out txt echoed,
)
{
    return (
        echoed = self.ref,
    )
}

call ECHO(
    ref = "/absent/mro2nf/ref.txt",
)
`

// TestOutsPublishStagedEntryFiles: the pipeline outs that return an entry
// file value read it in OUTS, so OUTS stages it, and post-processing
// publishes it where OUTS staged it, as mrp publishes the file it finds at
// the value: a link in outs/ the outs JSON names. A container sees only
// what its task stages.
func TestOutsPublishStagedEntryFiles(t *testing.T) {
	p := planOf(t, echoMRO)

	if d := cmp.Diff(PlanNode{Fqid: "ECHO", Files: []PlanFiles{{Leaves: []int{0}}}}, p.Outs); d != "" {
		t.Errorf("the pipeline outs' entry files (-want +plan):\n%s", d)
	}

	path := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(path, []byte(echoMRO), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := Load(path, p.Call, nil)
	if err != nil {
		t.Fatal(err)
	}

	if readers, err := g.EntryFileReaders(g.Manifest()); err != nil || !readers["ECHO"] {
		t.Errorf("entry file readers %v (%v), want the pipeline ECHO among them", readers, err)
	}

	t.Chdir(t.TempDir())

	if err := os.MkdirAll("entry/1", 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile("entry/1/ref.txt", []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := g.StagedEntryFiles(g.Manifest(), "entry", []int{0})
	if err != nil {
		t.Fatal(err)
	}

	spec := OutsSpec{EntryFiles: files, Manifest: g.Manifest(), Work: "work", Results: "results", OutsJSON: "pipeline_outs.json"}
	if err := g.WriteOuts(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	var got map[string]any

	raw, err := os.ReadFile("pipeline_outs.json")
	if err == nil {
		err = json.Unmarshal(raw, &got)
	}

	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(map[string]any{"echoed": "echoed.txt"}, got); d != "" {
		t.Errorf("pipeline_outs.json (-want +got):\n%s", d)
	}

	if d := cmp.Diff(map[string]string{"echoed.txt": "ref"}, publishedFiles(t, "results")); d != "" {
		t.Errorf("published tree (-want +got):\n%s", d)
	}
}
