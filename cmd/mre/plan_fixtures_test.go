package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPlanEveryFixture: mre plan starts a run of every fixture's transpiled
// invocation, writing one nodes line per witness node, and refuses the
// fixtures mrp refuses to instantiate (#387); the test then recompiles the
// call file it wrote and writes every entry-fed root's fork elements from it,
// as PLAN does.
func TestPlanEveryFixture(t *testing.T) {
	refused := []string{"rt_gate_split_reject", "rt_gate_split_reject_pass", "rt_gate_split_reject_sub"}

	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "pipeline.mro"))
	if err != nil || len(files) < 100 {
		t.Fatalf("fixtures: %d, %v", len(files), err)
	}

	for _, mro := range files {
		name := filepath.Base(filepath.Dir(mro))
		if slices.Contains(refused, name) {
			continue
		}

		t.Run(name, func(t *testing.T) {
			work, err := plan(t, name, "{}")
			if err != nil {
				t.Fatal(err)
			}

			nodes := readText(t, filepath.Join(work, "nodes"))
			for line := range strings.Lines(nodes) {
				if f := strings.Fields(line); len(f) < 3 {
					t.Errorf("nodes line %q: want fqid, digest and off", line)
				}
			}

			entryForks(t, graphFlags{entry: mro, call: filepath.Join(work, "call"), mropath: filepath.Dir(mro)}, filepath.Join(work, "entry"))
		})
	}
}
