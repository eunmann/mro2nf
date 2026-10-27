//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMrpDiffVDR runs fixtures under mrp's volatile data removal, instead of
// the --vdrmode=disable the stage-ABI differential needs, and requires the
// published outputs to match mro2nf's, which deletes nothing: vdr_volatile
// under the default rolling mode and strict mode, and Martian's retain_test
// under the strict mode its run_test.sh gives it. Each mrp run must have
// deleted a file, so the match shows VDR left the outputs alone rather than
// that it did nothing.
func TestMrpDiffVDR(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	for _, tc := range []struct {
		fixture string
		modes   []string
	}{
		{fixture: "vdr_volatile", modes: []string{"rolling", "strict"}},
		{fixture: "retain_test", modes: []string{"strict"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			proj := transpile(t, tc.fixture)
			if err := runNextflow(t, proj); err != nil {
				t.Fatal(err)
			}

			for _, mode := range tc.modes {
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					diffVDRMode(t, bin, tc.fixture, mode, proj)
				})
			}
		})
	}
}

// diffVDRMode runs fixture under mrp --vdrmode=mode and compares its
// published outputs with the Nextflow project proj's.
func diffVDRMode(t *testing.T, bin, fixture, mode, proj string) {
	t.Helper()

	tmp := t.TempDir()
	if out, err := execMrpVDR(t.Context(), bin, filepath.Join(root, "testdata", fixture), "pipeline.mro", tmp, mode, nil); err != nil {
		t.Fatalf("mrp --vdrmode=%s: %v\n%s", mode, err, tail(out, 12))
	}

	if n := vdrDeletedFiles(t, filepath.Join(tmp, "mrp")); n == 0 {
		t.Errorf("mrp --vdrmode=%s deleted no file, so the comparison shows nothing about VDR", mode)
	}

	compareMrpToNextflow(t, fixture, tmp, proj)
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
