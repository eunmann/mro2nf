//go:build e2e

package e2e

// Unit coverage for the shipped Groovy helper library (lib/Mro2nf.groovy, #49):
// a probe workflow exercises each helper against fixture JSON, run with the same
// Nextflow the rest of the suite uses. This pins the helpers' behavior directly,
// independent of a full pipeline run, so a regression in a helper is a precise
// failure rather than a mysterious downstream one.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLibHelpers exercises each Mro2nf helper against fixture JSON via a probe
// workflow, pinning their behavior directly (independent of a full pipeline).
func TestLibHelpers(t *testing.T) {
	requireTools(t, "nextflow", "java")

	// Transpile any fixture to obtain a project whose lib/ carries Mro2nf.groovy.
	proj := transpile(t, "file_min")

	// Probe bundles: disable gate (true/false), a chunk with resources, and a
	// keyed FORKBIND-style dir enumerated from forknames.json.
	writeBundleSidecar(t, filepath.Join(proj, "probe_on"), `{"disabled": true}`)
	writeBundleSidecar(t, filepath.Join(proj, "probe_off"), `{"disabled": false}`)
	writeBundleSidecar(t, filepath.Join(proj, "probe_chunk"), `{"resources": {"threads": 3}}`)
	writeFileT(t, filepath.Join(proj, "probe_join.json"), `{"mem_gb": 7}`)

	forks := filepath.Join(proj, "probe_forks")
	if err := os.MkdirAll(forks, 0o755); err != nil {
		t.Fatalf("mkdir forks: %v", err)
	}

	writeFileT(t, filepath.Join(forks, "forknames.json"), `["fa","fb"]`)
	writeFileT(t, filepath.Join(forks, "fa"), "")
	writeFileT(t, filepath.Join(forks, "fb"), "")

	probe := `workflow {
    def on = file("${projectDir}/probe_on")
    def off = file("${projectDir}/probe_off")
    def chunk = file("${projectDir}/probe_chunk")
    def joinf = file("${projectDir}/probe_join.json")
    def forks = file("${projectDir}/probe_forks")
    println "R disabled=" + Mro2nf.disabled(on) + "/" + Mro2nf.disabled(off)
    println "R chunkRes=" + Mro2nf.chunkRes(chunk).threads
    println "R parseJson=" + Mro2nf.parseJson(joinf).mem_gb
    println "R asList=" + Mro2nf.asList(5) + "/" + Mro2nf.asList([1, 2])
    println "R outerKey=" + Mro2nf.outerKey("a~b~c")
    println "R forkKeys=" + Mro2nf.forkTuples("K", forks).collect { it[0] }.join(",")
}
`
	writeFileT(t, filepath.Join(proj, "probe.nf"), probe)

	out := runNextflowCapture(t, proj, "probe.nf")

	for _, want := range []string{
		"R disabled=true/false",
		"R chunkRes=3",
		"R parseJson=7",
		"R asList=[5]/[1, 2]",
		"R outerKey=a~b",
		"R forkKeys=K~fa,K~fb",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("helper probe missing %q in:\n%s", want, out)
		}
	}
}

// runNextflowCapture runs `nextflow run <script>` in proj and returns the
// combined output (the driver's stdout carries a probe workflow's println).
func runNextflowCapture(t *testing.T, proj, script string) string {
	t.Helper()

	cmd := exec.Command("nextflow", "run", script)
	cmd.Dir = proj

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow run %s: %v\n%s", script, err, out)
	}

	return string(out)
}

// writeBundleSidecar creates dir/data.json with the given JSON — the minimal
// bundle shape the driver-side helpers read.
func writeBundleSidecar(t *testing.T, dir, dataJSON string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	writeFileT(t, filepath.Join(dir, "data.json"), dataJSON)
}
