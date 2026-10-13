//go:build e2e

package e2e

import (
	"encoding/json"
	"path/filepath"
	"strconv"
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

// TestEntryRootsMatchMrp (#509): a pipeline of 300 entry-fed fork roots
// runs to mrp's outputs and stage _args: PLAN's one record of every root's
// forks, and one of their counts, reach each root's instances and sized
// gathers as a record per root did. Most roots are disabled, so they fork
// but run no task under either runner. PLAN declared two outputs per root
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
