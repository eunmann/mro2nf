package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
)

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
	if err := writeChunksDir(dir, defs, types.Manifest{}, "S", nil); err != nil {
		t.Fatal(err)
	}

	got, names, err := readChunksDir(dir)
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

	c, err := readChunk(filepath.Join(dir, names[3]))
	if err != nil {
		t.Fatal(err)
	}

	if c.Index != 3 || c.Count != 11 {
		t.Errorf("chunk %s is %d of %d, want 3 of 11", names[3], c.Index, c.Count)
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

	file := []ir.Param{{Name: "f", BaseType: "txt", IsFile: true}}
	man := types.Manifest{Callables: map[string]types.Callable{"S": {Out: file, ChunkIn: file}}}
	outs := json.RawMessage(`{"f":"` + host + `"}`)
	defs := &core.StageDefs{ChunkDefs: []*core.ChunkDef{{Args: core.LazyArgumentMap{"f": json.RawMessage(`"` + host + `"`)}}}}

	for _, strict := range []bool{false, true} {
		f := runFlags{confine: strict, rt: job.Runtime{Work: work}}

		mainConf, err := f.confinement(job.ChunkName(0, 1))
		if err != nil {
			t.Fatal(err)
		}

		splitConf, err := f.confinement(job.PhaseSplit)
		if err != nil {
			t.Fatal(err)
		}

		outsDir, chunksDir := filepath.Join(root, "outs", strconv.FormatBool(strict)), filepath.Join(root, "chunks", strconv.FormatBool(strict))
		errs := []error{
			writeOutsBundle(outsDir, outs, man, "S", types.RoleOut, mainConf),
			writeChunksDir(chunksDir, defs, man, "S", splitConf),
		}

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

	m, err := readArgsMap(dir)
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
