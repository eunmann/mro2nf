package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/eunmann/mro2nf/internal/fileio"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/eunmann/mro2nf/internal/types/typestest"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// untypedWriter writes bundles whose params name no file.
func untypedWriter() itemWriter {
	return itemWriter{tbl: types.NewTable(syntax.NewTypeLookup())}
}

// TestOutsMustBeAnObject: a job's _outs that is an array or a number fails
// a stage with outputs, its post and its chunk's outs bundle, as mrp fails
// a stage whose _outs does not decode as an object (core/stage.go:1282
// doComplete, :1214 doJoin, metadata.go:640 read); null and the adapter's
// "" carry no outs, and a stage with no outputs has none whatever its job
// wrote, as mrp writes it {} (core/stage.go:1292).
func TestOutsMustBeAnObject(t *testing.T) {
	outs := &syntax.OutParams{List: []*syntax.OutParam{{Id: "n", Tname: syntax.TypeId{Tname: syntax.KindInt}}}}
	params := []*syntax.StructMember{&outs.List[0].StructMember}

	for _, raw := range []string{`[1]`, `5`} {
		if _, err := finishedOf(json.RawMessage(raw), outs, shim.Opts{}); !errors.Is(err, errNotObject) {
			t.Errorf("finishedOf(%s): %v, want %v", raw, err, errNotObject)
		}

		if _, err := writeOutsBundle(t.TempDir(), json.RawMessage(raw), params, untypedWriter()); !errors.Is(err, errNotObject) {
			t.Errorf("writeOutsBundle(%s): %v, want %v", raw, err, errNotObject)
		}

		if f, err := finishedOf(json.RawMessage(raw), new(syntax.OutParams), shim.Opts{}); err != nil || len(f.outs) != 0 {
			t.Errorf("finishedOf(%s) of a stage with no outputs = %v, %v; want no outs", raw, f.outs, err)
		}
	}

	for _, raw := range []string{`null`, `""`} {
		if f, err := finishedOf(json.RawMessage(raw), outs, shim.Opts{}); err != nil || len(f.outs) != 0 {
			t.Errorf("finishedOf(%s) = %v, %v; want no outs", raw, f.outs, err)
		}
	}
}

// TestChunksDirRoundTrip: the chunks dir a split's job writes reads back as
// the split's stage defs, in chunk order under mrp's chunk names, each
// def's args and resources and the join's resources intact.
func TestChunksDirRoundTrip(t *testing.T) {
	defs := &core.StageDefs{JoinDef: &core.JobResources{Threads: 2, MemGB: 3}}
	for i := range 11 {
		res := &core.JobResources{MemGB: float64(i + 1)}
		defs.ChunkDefs = append(defs.ChunkDefs, &core.ChunkDef{Args: core.LazyArgumentMap{"i": json.RawMessage([]byte{'0' + byte(i%10)})}, Resources: res})
	}

	dir := filepath.Join(t.TempDir(), "chunks")
	if _, err := writeChunksDir(dir, defs, nil, untypedWriter()); err != nil {
		t.Fatal(err)
	}

	got, names, err := readChunksDir(dir, shim.Refs{})
	if err != nil {
		t.Fatal(err)
	}

	if names[0] != "chnk00" || names[10] != "chnk10" || len(names) != 11 {
		t.Errorf("chunk names %v, mrp names 11 chunks chnk00..chnk10", names)
	}

	want, err := json.Marshal(defs)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(string(want), string(raw)); d != "" {
		t.Errorf("stage defs differ after the round trip (-split +read):\n%s", d)
	}

	c, err := readChunk(filepath.Join(dir, names[3]), shim.Refs{})
	if err != nil {
		t.Fatal(err)
	}

	if c.Index != 3 || c.Count != 11 {
		t.Errorf("chunk %s is %d of %d, want 3 of 11", names[3], c.Index, c.Count)
	}
}

// TestWriteResources: the resources table holds, per chunk and for the join,
// the cpus, memory and special its job's _jobinfo records: a def's request
// over the stage's, a fractional or negative count at its rounded-up
// magnitude (#405, #315), from a local pool at most the pool (#545), and
// under the run's overrides, theirs over both. A special holding a line
// break has no row.
func TestWriteResources(t *testing.T) {
	st := job.Stage{Fqid: "P.S", Split: true, Using: &core.JobResources{Threads: 2, MemGB: 4, Special: "gpu:2"}}
	defs := &core.StageDefs{
		ChunkDefs: []*core.ChunkDef{
			{},
			{Resources: &core.JobResources{Threads: 3.5, Special: "gpu"}},
			{Resources: &core.JobResources{Threads: -6, MemGB: 0.5}},
		},
		JoinDef: &core.JobResources{MemGB: 3},
	}

	over := filepath.Join(t.TempDir(), "overrides.json")
	writeTestFile(t, over, `{"P": {"chunk.threads": 1.5, "join.mem_gb": 2}, "P.S": {"split.threads": 8}}`)

	overrides, err := core.ReadOverrides(over)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		rt   job.Runtime
		want string
	}{
		{"requests", job.Runtime{PSID: "ps"}, "chnk0\t2\t4\tgpu:2\nchnk1\t4\t4\tgpu\nchnk2\t6\t0.5\tgpu:2\njoin\t2\t3\tgpu:2\n"},
		{"pooled", job.Runtime{PSID: "ps", Local: job.Pool{Cores: 2, MemGB: 3}}, "chnk0\t2\t3\tgpu:2\nchnk1\t2\t3\tgpu\nchnk2\t2\t0.5\tgpu:2\njoin\t2\t3\tgpu:2\n"},
		{"overridden", job.Runtime{PSID: "ps", Overrides: overrides}, "chnk0\t2\t4\tgpu:2\nchnk1\t2\t4\tgpu\nchnk2\t2\t0.5\tgpu:2\njoin\t2\t2\tgpu:2\n"},
	} {
		path := filepath.Join(t.TempDir(), resourcesTable)
		if err := writeResources(path, tc.rt, st, defs); err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff(tc.want, readText(t, path)); d != "" {
			t.Errorf("%s: resources table (-want +got):\n%s", tc.name, d)
		}
	}

	defs.JoinDef.Special = "a\nb"
	if err := writeResources(filepath.Join(t.TempDir(), resourcesTable), job.Runtime{}, st, defs); !errors.Is(err, errSpecialLine) {
		t.Errorf("writeResources = %v, want %v", err, errSpecialLine)
	}
}

// TestChunksDirStagedAsLinks: a join task stages the split's chunk dirs as
// links, as Nextflow stages a path input, and reads every chunk through them.
func TestChunksDirStagedAsLinks(t *testing.T) {
	defs := &core.StageDefs{JoinDef: &core.JobResources{}}
	for i := range 2 {
		defs.ChunkDefs = append(defs.ChunkDefs, &core.ChunkDef{Args: core.LazyArgumentMap{"i": json.RawMessage(strconv.Itoa(i))}})
	}

	split := filepath.Join(t.TempDir(), "chunks")
	if _, err := writeChunksDir(split, defs, nil, untypedWriter()); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(t.TempDir(), "chunks")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"chnk0", "chnk1", joinRecord} {
		if err := os.Symlink(filepath.Join(split, name), filepath.Join(staged, name)); err != nil {
			t.Fatal(err)
		}
	}

	got, names, err := readChunksDir(staged, shim.Refs{})
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff([]string{"chnk0", "chnk1"}, names); d != "" || len(got.ChunkDefs) != 2 {
		t.Errorf("chunks read through links (-want +got):\n%s", d)
	}
}

// TestChunksDirRefusesMissingChunks: a join that cannot read every chunk the
// split returned fails instead of joining a subset: a staged chunk link that
// does not resolve, or chunks fewer than their records count.
func TestChunksDirRefusesMissingChunks(t *testing.T) {
	defs := &core.StageDefs{JoinDef: &core.JobResources{}}
	for i := range 3 {
		defs.ChunkDefs = append(defs.ChunkDefs, &core.ChunkDef{Args: core.LazyArgumentMap{"i": json.RawMessage(strconv.Itoa(i))}})
	}

	split := filepath.Join(t.TempDir(), "chunks")
	if _, err := writeChunksDir(split, defs, nil, untypedWriter()); err != nil {
		t.Fatal(err)
	}

	for name, link := range map[string]func(string) string{
		"dangling link": func(n string) string {
			if n == "chnk1" {
				return filepath.Join(split, "gone")
			}

			return filepath.Join(split, n)
		},
		"chunk not staged": func(n string) string {
			if n == "chnk1" {
				return ""
			}

			return filepath.Join(split, n)
		},
	} {
		staged := filepath.Join(t.TempDir(), "chunks")
		if err := os.MkdirAll(staged, 0o755); err != nil {
			t.Fatal(err)
		}

		for _, n := range []string{"chnk0", "chnk1", "chnk2", joinRecord} {
			if target := link(n); target != "" {
				if err := os.Symlink(target, filepath.Join(staged, n)); err != nil {
					t.Fatal(err)
				}
			}
		}

		if _, names, err := readChunksDir(staged, shim.Refs{}); err == nil {
			t.Errorf("%s: read chunks %v of 3, want an error", name, names)
		}
	}
}

// TestRunStampsStage: a job's metadata dir names its stage before the job
// runs, where the mrp ABI differential and failure attribution read it. The
// stamp creates the dir, so it gives the dir the job's mode: the job's own
// MkdirAll leaves an existing dir's mode as it is.
func TestRunStampsStage(t *testing.T) {
	umask := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(umask) })

	f := runFlags{rt: job.Runtime{Work: t.TempDir()}}
	in := job.Instance{Stage: job.Stage{Callable: "SUM_SQUARES"}}
	meta := filepath.Join(f.rt.Work, job.ChunkName(0, 1))

	if err := f.stamp(in, job.ChunkName(0, 1)); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(meta, "_mre_stage"))
	if err != nil || string(got) != "SUM_SQUARES" {
		t.Errorf("stage stamp %q (%v), want SUM_SQUARES", got, err)
	}

	fi, err := os.Stat(meta)
	if err != nil {
		t.Fatal(err)
	}

	if fi.Mode().Perm() != fileio.DirPerm {
		t.Errorf("metadata dir mode %v, want %v", fi.Mode().Perm(), os.FileMode(fileio.DirPerm))
	}
}

// TestRunBundlesConfineStageOutputs: a file a job's _outs or chunk defs name
// outside the task work dir is copied into the bundle, never hard-linked, and
// refused under -confine, as the stage phases' bundles are.
func TestRunBundlesConfineStageOutputs(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "host.txt")
	work := filepath.Join(root, "work")

	if err := os.WriteFile(host, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, sub := range []string{job.PhaseSplit, job.ChunkName(0, 1)} {
		if err := os.MkdirAll(filepath.Join(work, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tbl, file := compileTypes(t, "filetype txt;\n"), typestest.Fields(t, "f txt")
	outs := json.RawMessage(`{"f":"` + host + `"}`)
	defs := &core.StageDefs{ChunkDefs: []*core.ChunkDef{{Args: core.LazyArgumentMap{"f": json.RawMessage(`"` + host + `"`)}}}}

	for _, strict := range []bool{false, true} {
		f := runFlags{confine: strict, rt: job.Runtime{Work: work}}

		mainOpts, err := f.bundleOpts(job.ChunkName(0, 1))
		if err != nil {
			t.Fatal(err)
		}

		splitOpts, err := f.bundleOpts(job.PhaseSplit)
		if err != nil {
			t.Fatal(err)
		}

		outsDir, chunksDir := filepath.Join(root, "outs", strconv.FormatBool(strict)), filepath.Join(root, "chunks", strconv.FormatBool(strict))
		_, outsErr := writeOutsBundle(outsDir, outs, file, itemWriter{tbl: tbl, opts: mainOpts})
		_, chunksErr := writeChunksDir(chunksDir, defs, file, itemWriter{tbl: tbl, opts: splitOpts})
		errs := []error{outsErr, chunksErr}

		for i, dir := range []string{outsDir, filepath.Join(chunksDir, job.ChunkName(0, 1))} {
			checkConfinedLeaf(t, dir, host, strict, errs[i])
		}
	}
}

// checkConfinedLeaf requires the bundle write that returned err to have
// refused host under strict, and otherwise to hold a copy of it in dir.
func checkConfinedLeaf(t *testing.T, dir, host string, strict bool, err error) {
	t.Helper()

	if strict {
		if err == nil {
			t.Errorf("%s: -confine wrote a bundle naming the host file %s, want it refused", dir, host)
		}

		return
	}

	if err != nil {
		t.Fatal(err)
	}

	m, err := readArgsMap(dir, shim.Refs{})
	if err != nil {
		t.Fatal(err)
	}

	var staged string
	if err := json.Unmarshal(m["f"], &staged); err != nil {
		t.Fatal(err)
	}

	hi, err := os.Stat(host)
	if err != nil {
		t.Fatal(err)
	}

	si, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}

	if os.SameFile(hi, si) {
		t.Errorf("%s: the host file %s is hard-linked into the bundle as %s, want a copy", dir, host, staged)
	}
}
