//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/test/wide"
	"github.com/google/go-cmp/cmp"
)

// TestWidePipelineRuns (#503): the project emitted for a pipeline of over
// 500 stage calls of every shape the emitter wires compiles under
// Nextflow, where one workflow of them all passed the JVM's 64 KiB limit
// on a method, and runs to the outputs its stages compute.
func TestWidePipelineRuns(t *testing.T) {
	t.Parallel()

	runWide(t, wide.New(32, 150, 0))
}

// TestLaunchEdges (#559): Nextflow adds each edge of its DAG in time
// linear in the edges before it (DAG.findEdge), so its launch is quadratic
// in them, and a few edges per data dependency of a pipeline (a read, a
// gate, a fork root, a split's phases, an output) cost CellRanger's
// -preview seconds per hundred. The bounds are the edges -preview builds,
// operators included (dag.verbose), under Nextflow 25.10 and 26.04 alike,
// of the wide pipeline and of cellranger_shaped, which were 1,221 and 346
// before #559: an edge more per dependency fails them.
func TestLaunchEdges(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	dir := t.TempDir()
	if err := wide.New(2, 3, 10).Write(dir); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, dir string
		bound     int
	}{
		{"wide", dir, 951},
		{"cellranger_shaped", filepath.Join(root, "testdata", "cellranger_shaped"), 268},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := launchEdges(t, transpileDir(t, c.dir)); got > c.bound {
				t.Errorf("-preview builds %d edges, want at most %d", got, c.bound)
			}
		})
	}
}

// launchEdges is the edges of the DAG Nextflow builds to launch proj,
// operators included.
func launchEdges(t *testing.T, proj string) int {
	t.Helper()

	if err := os.WriteFile(filepath.Join(proj, "verbose.config"), []byte("dag.verbose = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := execNextflow(t, proj, []string{"-preview", "-c", "verbose.config", "-with-dag", "dag.mmd"}); err != nil {
		t.Fatalf("nextflow run -preview: %v", err)
	}

	dag, err := os.ReadFile(filepath.Join(proj, "dag.mmd"))
	if err != nil {
		t.Fatal(err)
	}

	return strings.Count(string(dag), "-->")
}

// TestEntryRootsMatchMrp (#509): a pipeline of 300 entry-fed fork roots
// runs to mrp's outputs and stage _args: PLAN's one record of every root's
// forks reaches each root's instances as a record per root did. Most roots
// are disabled, so they fork but run no task under either runner. PLAN
// declared two outputs per root
// and outgrew the JVM's limit on a method past about 1,100 roots, whose
// pipeline's 1,100 processes take the driver minutes to create;
// TestEmitBoundsPlan pins PLAN's outputs and wiring at 600.
func TestEntryRootsMatchMrp(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	shardSkip(t)

	p := wide.Roots(8, 280, 6)
	if p.Roots < 300 {
		t.Fatalf("the pipeline has %d entry-fed roots, want at least 300", p.Roots)
	}

	dir := t.TempDir()
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	runMrpAt(t, bin, dir, "pipeline.mro", tmp)

	proj := transpileDir(t, dir)
	if err := runNextflow(t, proj, mrpPoolArgs(t, proj)...); err != nil {
		t.Fatalf("nextflow run: %v", err)
	}

	compareMrpToNextflow(t, "entry_roots", tmp, proj)
	compareStageABI(t, "entry_roots", tmp, proj)
}

// runWide writes p, transpiles and runs it under Nextflow, and checks the
// pipeline outs its stages compute.
func runWide(t *testing.T, p wide.Pipeline) {
	t.Helper()

	requireTools(t, "nextflow", "java", "python3")

	if p.Stages < 500 {
		t.Fatalf("the wide pipeline has %d stage calls, want at least 500", p.Stages)
	}

	dir := t.TempDir()
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}

	proj := transpileDir(t, dir)
	if err := runNextflow(t, proj); err != nil {
		t.Fatalf("nextflow run: %v", err)
	}

	var outs map[string]any

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	want := map[string]any{"all": json.Number(strconv.Itoa(p.All))}
	if len(p.Rs) > 0 {
		rs := make([]any, len(p.Rs))
		for i, r := range p.Rs {
			rs[i] = json.Number(strconv.Itoa(r))
		}

		want["rs"] = rs
	}

	if d := cmp.Diff(want, outs); d != "" {
		t.Errorf("pipeline outs (-want +got):\n%s", d)
	}
}
