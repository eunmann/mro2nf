//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMrpDiffVDR runs vdr_volatile under mrp's volatile data removal, its
// default rolling mode and strict mode, instead of the --vdrmode=disable the
// stage-ABI differential needs, and requires the published outputs to match
// mro2nf's, which deletes nothing. Each mrp run must have deleted a file, so
// the match shows VDR left the outputs alone rather than that it did nothing.
func TestMrpDiffVDR(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	shardSkip(t)

	proj := transpile(t, "vdr_volatile")
	if err := runNextflow(t, proj); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"rolling", "strict"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			if out, err := execMrpVDR(bin, filepath.Join(root, "testdata", "vdr_volatile"), "pipeline.mro", tmp, mode); err != nil {
				t.Fatalf("mrp --vdrmode=%s: %v\n%s", mode, err, tail(out, 12))
			}

			if n := vdrDeletedFiles(t, filepath.Join(tmp, "mrp")); n == 0 {
				t.Errorf("mrp --vdrmode=%s deleted no file, so the comparison shows nothing about VDR", mode)
			}

			compareMrpToNextflow(t, "vdr_volatile", tmp, proj)
		})
	}
}

// vdrDeletedFiles is the number of files mrp's VDR deleted in the pipestance
// ps, from the kill report mrp writes at its root.
func vdrDeletedFiles(t *testing.T, ps string) int {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(ps, "_vdrkill"))
	if err != nil {
		t.Fatal(err)
	}

	var report struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse %s/_vdrkill: %v", ps, err)
	}

	return report.Count
}
