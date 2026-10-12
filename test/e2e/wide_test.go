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
// on a method, and runs to the output its stages compute.
func TestWidePipelineRuns(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	p := wide.New(32, 150)
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

	var outs map[string]json.Number

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	if d := cmp.Diff(map[string]json.Number{"all": json.Number(strconv.Itoa(p.All))}, outs); d != "" {
		t.Errorf("pipeline outs (-want +got):\n%s", d)
	}
}
