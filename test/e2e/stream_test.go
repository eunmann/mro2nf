//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streamMro maps SAMPLE over two samples. Each merges its own inner map's
// WORK forks in TOTAL; the slow sample's WORK runs well past the fast
// sample's whole chain.
const streamMro = `stage MKLIST(
    in  string id,
    out int[]  xs,
    src py     "stages/mklist",
)

stage WORK(
    in  string id,
    in  int    x,
    out int    y,
    src py     "stages/work",
)

stage TOTAL(
    in  string id,
    in  int[]  ys,
    out int    s,
    src py     "stages/total",
)

pipeline SAMPLE(
    in  string id,
    out int    s,
)
{
    call MKLIST(
        id = self.id,
    )

    map call WORK(
        id = self.id,
        x  = split MKLIST.xs,
    )

    call TOTAL(
        id = self.id,
        ys = WORK.y,
    )

    return (
        s = TOTAL.s,
    )
}

pipeline STREAM(
    in  string[] ids,
    out int[]    ss,
)
{
    map call SAMPLE(
        id = split self.ids,
    )

    return (
        ss = SAMPLE.s,
    )
}

call STREAM(
    ids = ["fast", "slow"],
)
`

// TestForkedGatherStreams: a forked call's gather of its own share of an
// inner map call starts once that share is done, not once every fork's is
// (#259). The trace shows it: the fast sample's TOTAL is submitted before
// the slow sample's WORK completes, which a gather waiting for every WORK
// can never do. The assertion reads only the driver's submit times, so the
// number of tasks the host runs at once doesn't change it. mrp holds a
// node until every fork of each node it reads is done (core/node.go:646
// getState), so this is no mrp fixture.
func TestForkedGatherStreams(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	dir := t.TempDir()

	stages := map[string]string{
		"mklist": "def main(args, outs):\n    outs.xs = [1, 2]\n",
		"work": "import time\n\n\ndef main(args, outs):\n" +
			"    if args.id == 'slow':\n" +
			"        time.sleep(20)\n" +
			"    outs.y = args.x\n",
		"total": "def main(args, outs):\n    outs.s = sum(args.ys)\n",
	}

	for name, code := range stages {
		if err := os.MkdirAll(filepath.Join(dir, "stages", name), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, "stages", name, "__init__.py"), []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(streamMro), 0o644); err != nil {
		t.Fatal(err)
	}

	proj := transpileMRO(t, dir, "pipeline.mro")

	// The slow WORKs hold their slots while they sleep, so the fast sample
	// needs slots beyond them; the local executor's defaults are the host's
	// cores, which on a CI runner the slow WORKs could fill.
	cfg := filepath.Join(proj, "stream.config")
	if err := os.WriteFile(cfg, []byte("executor.cpus = 8\nexecutor.queueSize = 8\n"+
		"trace.fields = 'task_id,hash,name,status,exit,submit,complete'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runNextflow(t, proj, "-c", cfg, "-with-trace", "stream-trace.txt"); err != nil {
		t.Fatal(err)
	}

	fastTotal, slowWork := streamTimes(t, filepath.Join(proj, "stream-trace.txt"))
	if fastTotal == "" || slowWork == "" || fastTotal >= slowWork {
		t.Errorf("the fast sample's TOTAL was submitted at %q, not before the slow sample's WORK completed at %q", fastTotal, slowWork)
	}

	var outs struct {
		SS []int `json:"ss"`
	}

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	if len(outs.SS) != 2 || outs.SS[0] != 3 || outs.SS[1] != 3 {
		t.Errorf("ss = %v, want [3 3]", outs.SS)
	}
}

// streamTimes returns when the fast sample's TOTAL was submitted and when
// the slow sample's last WORK completed, as the trace's sortable timestamps.
func streamTimes(t *testing.T, trace string) (string, string) {
	t.Helper()

	fastTotal, slowWork := "", ""

	for _, r := range readTraceColumns(t, trace, "name", "submit", "complete") {
		switch {
		case strings.Contains(r[0], "TOTAL 0"):
			fastTotal = r[1]
		case strings.Contains(r[0], "WORK 1.") && r[2] > slowWork:
			slowWork = r[2]
		}
	}

	return fastTotal, slowWork
}
