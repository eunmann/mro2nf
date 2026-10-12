//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGPURequestFollowsSpecial runs the generated config's accelerator rule
// on the meta the stage module schedules each task with (#315): a stage's
// own `special = "gpu[:N]"` requests N GPUs, as does a chunk's or join's
// special in the split's resources table (TestWriteResources writes the
// split's __special there, over the stage's); any other special requests
// none.
func TestGPURequestFollowsSpecial(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "gpu_stage")

	probe := `include { resources } from './stage.nf'

process GPU_PROBE {
    input:
    val meta

    output:
    stdout

    script:
    "echo R ${meta.name}=${task.accelerator?.request}"
}

workflow {
    def gpu2 = [name: 'stage', cpus: 1, mem_gb: 1, special: 'gpu:2']
    def none = [name: 'none', cpus: 1, mem_gb: 1, special: '']
    def r = resources("chnk0\t2\t1\tgpu:2\nchnk1\t1\t1\tgpu\nchnk2\t1\t1\thighmem\njoin\t1\t3\tgpu:3\n")
    GPU_PROBE(channel.of(
        gpu2,
        none,
        none + r.chnk0 + [name: 'chunk_keeps'],
        none + r.chnk1 + [name: 'chunk'],
        gpu2 + r.chnk2 + [name: 'other'],
        none + r.join + [name: 'join'],
        none + [name: 'zero', special: 'gpu:0'],
    )).view()
}
`
	if err := os.WriteFile(filepath.Join(proj, "probe.nf"), []byte(probe), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("nextflow", "run", "probe.nf")
	cmd.Dir = proj

	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow run probe.nf: %v\n%s", err, raw)
	}

	out := string(raw)

	for _, want := range []string{
		"R stage=2", "R none=null", "R chunk_keeps=2", "R chunk=1", "R join=3", "R other=null", "R zero=null",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GPU probe lacks %q in:\n%s", want, out)
		}
	}
}
