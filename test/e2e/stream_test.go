//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streamMro maps SAMPLE over two samples. Each merges its own inner map's
// WORK forks in TOTAL; the slow sample's WORK waits for the fast sample's
// TOTAL to have run, and fails if it does not within the timeout.
const streamMro = `stage MKLIST(
    in  string id,
    out int[]  xs,
    src py     "stages/mklist",
)

stage WORK(
    in  string id,
    in  int    x,
    in  string sync,
    out int    y,
    src py     "stages/work",
)

stage TOTAL(
    in  string id,
    in  int[]  ys,
    in  string sync,
    out int    s,
    src py     "stages/total",
)

pipeline SAMPLE(
    in  string id,
    in  string sync,
    out int    s,
)
{
    call MKLIST(
        id = self.id,
    )

    map call WORK(
        id   = self.id,
        x    = split MKLIST.xs,
        sync = self.sync,
    )

    call TOTAL(
        id   = self.id,
        ys   = WORK.y,
        sync = self.sync,
    )

    return (
        s = TOTAL.s,
    )
}

pipeline STREAM(
    in  string[] ids,
    in  string   sync,
    out int[]    ss,
)
{
    map call SAMPLE(
        id   = split self.ids,
        sync = self.sync,
    )

    return (
        ss = SAMPLE.s,
    )
}

call STREAM(
    ids  = ["fast", "slow"],
    sync = "SYNC",
)
`

// TestForkedGatherStreams: a forked call's gather of its own share of an
// inner map call starts once that share is done, not once every fork's is
// (#259): the slow sample's WORK finishes only after the fast sample's
// TOTAL has run, which a gather waiting for every WORK would never let
// happen. mrp holds a node until every fork of each node it reads is done
// (core/node.go:646 getState), so the program deadlocks under mrp and is
// no mrp fixture.
func TestForkedGatherStreams(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	dir, sync := t.TempDir(), t.TempDir()
	stages := map[string]string{
		"mklist": "def main(args, outs):\n    outs.xs = [1, 2]\n",
		"work": "import os\nimport time\n\n\ndef main(args, outs):\n" +
			"    if args.id == 'slow':\n" +
			"        done = os.path.join(args.sync, 'fast.done')\n" +
			"        for _ in range(600):\n" +
			"            if os.path.exists(done):\n" +
			"                break\n" +
			"            time.sleep(0.1)\n" +
			"        else:\n" +
			"            raise RuntimeError('the fast sample did not finish first')\n" +
			"    outs.y = args.x\n",
		"total": "import os\n\n\ndef main(args, outs):\n" +
			"    open(os.path.join(args.sync, args.id + '.done'), 'w').close()\n" +
			"    outs.s = sum(args.ys)\n",
	}

	for name, code := range stages {
		if err := os.MkdirAll(filepath.Join(dir, "stages", name), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, "stages", name, "__init__.py"), []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(strings.Replace(streamMro, "SYNC", sync, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	proj := transpileMRO(t, dir, "pipeline.mro")
	if err := runNextflow(t, proj); err != nil {
		t.Fatalf("the slow sample's WORK waited for the fast sample's TOTAL, which waited for it: %v", err)
	}

	var outs struct {
		SS []int `json:"ss"`
	}

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	if len(outs.SS) != 2 || outs.SS[0] != 3 || outs.SS[1] != 3 {
		t.Errorf("ss = %v, want [3 3]", outs.SS)
	}
}
