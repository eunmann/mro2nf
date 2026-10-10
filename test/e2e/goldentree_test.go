//go:build e2e

package e2e

// Published-tree goldens (#256): testdata/<fixture>/expected/outs/ holds the
// file tree real mrp published for the fixture's invocation, so the golden
// suites check published contents and the published file set, not only the
// path strings in pipeline_outs.json. TestRecordGoldens is the only writer.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// goldenTreeDir is where, relative to the fixture dir, the mrp-published tree
// lives. A fixture without one published nothing under mrp.
const goldenTreeDir = "expected/outs"

const (
	unresolvedInput = "mrp runs the stage in its own job dir, where the invocation's relative input/ path does not resolve"
	launchOverride  = "launch-time -params-file override: mrp has no equivalent invocation to record"
)

// jsonOnlyGoldens are the goldenCases entries with no mrp-recorded tree,
// keyed by case name, each with the reason mrp cannot produce one. They are
// checked on pipeline_outs.json alone.
var jsonOnlyGoldens = map[string]string{
	"exec_min":                    "mrp never completes it: the exec stub writes no journal entries",
	"mixed_adapters":              "mrp never completes it: its stubs speak mre's wrapped-adapter contract, not mrp's journal protocol",
	"map_pipe_nested":             "mrp rejects it: OUTER.INNER.DBL's merge cannot match a reference to a specific fork",
	"entry_file":                  unresolvedInput,
	"entry_filearr":               unresolvedInput,
	"entry_struct_file":           unresolvedInput,
	"entry_mapfile":               unresolvedInput,
	"entry_dir":                   unresolvedInput,
	"split_from_file":             unresolvedInput,
	"entry_file_override":         launchOverride,
	"entry_filearr_override":      launchOverride,
	"entry_struct_file_override":  launchOverride,
	"entry_mapfile_override":      launchOverride,
	"entry_dir_override":          launchOverride,
	"split_from_file_override":    launchOverride,
	"empty_fork_min_override":     launchOverride,
	"empty_fork_sub_override":     launchOverride,
	"empty_fork_cascade_override": launchOverride,
}

// recordableFixtures lists, once each and in goldenCases order, the fixtures
// whose default invocation TestRecordGoldens records.
func recordableFixtures() []string {
	var fixtures []string

	for _, tc := range goldenCases {
		if _, jsonOnly := jsonOnlyGoldens[tc.name]; jsonOnly || slices.Contains(fixtures, tc.fixture) {
			continue
		}

		fixtures = append(fixtures, tc.fixture)
	}

	return fixtures
}

// assertGoldenTree requires a run's published results/ tree to equal the
// mrp-recorded expected/outs/ of golden case name: no file missing, none
// extra, every file's sha256 equal, and the same empty directories.
func assertGoldenTree(t *testing.T, name, fixture, proj string) {
	t.Helper()

	if _, jsonOnly := jsonOnlyGoldens[name]; jsonOnly {
		return
	}

	want := hashTree(t, filepath.Join(root, "testdata", fixture, filepath.FromSlash(goldenTreeDir)))
	got := hashTree(t, filepath.Join(proj, "results"), nfResultsMeta...)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published results/ differs from mrp's recorded %s/%s (-mrp +nextflow):\n%s",
			fixture, goldenTreeDir, diff)
	}
}

// TestRecordGoldens rewrites every recordable fixture's expected/outs/ from a
// real mrp run (make record-goldens). It replaces the tree wholesale, and
// writes none for a fixture whose mrp run published nothing.
func TestRecordGoldens(t *testing.T) {
	t.Parallel()

	if os.Getenv("GOLDEN_RECORD") != "1" {
		t.Skip("set GOLDEN_RECORD=1 (make record-goldens) to rewrite expected/outs/ from mrp")
	}

	bin, err := lookupMartianBin()
	if err != nil {
		t.Fatal(err)
	}

	for _, fixture := range recordableFixtures() {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			runMrp(t, bin, fixture, tmp)

			dst := filepath.Join(root, "testdata", fixture, goldenTreeDir)
			if err := recordTree(filepath.Join(tmp, "mrp", "outs"), dst); err != nil {
				t.Fatalf("record %s: %v", fixture, err)
			}
		})
	}
}

// errEmptyDirGolden reports an empty directory in mrp's tree: git cannot
// commit one, so the recorded golden would silently lose it.
var errEmptyDirGolden = errors.New("empty directory cannot be committed as a golden")

// recordTree replaces dst with copies of the files under mrp's outs dir src,
// minus mrp's root metadata, resolving symlinks to their contents.
func recordTree(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clear %s: %w", dst, err)
	}

	return walkPublished(src, mrpOutsMeta, func(rel, path string, emptyDir bool) error {
		if emptyDir {
			return fmt.Errorf("%s: %w", rel, errEmptyDirGolden)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}

		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", rel, err)
		}

		if err := os.WriteFile(out, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}

		return nil
	})
}

// TestGoldenTreeTables fails on a jsonOnlyGoldens entry naming no golden case,
// and on an expected/outs/ tree TestRecordGoldens does not write (one the
// recorder cannot reproduce from mrp).
func TestGoldenTreeTables(t *testing.T) {
	t.Parallel()

	for name := range jsonOnlyGoldens {
		if !slices.ContainsFunc(goldenCases, func(tc goldenCase) bool { return tc.name == name }) {
			t.Errorf("jsonOnlyGoldens lists %q, which is not a goldenCases entry", name)
		}
	}

	trees, err := filepath.Glob(filepath.Join(root, "testdata", "*", filepath.FromSlash(goldenTreeDir)))
	if err != nil {
		t.Fatal(err)
	}

	recordable := recordableFixtures()

	for _, tree := range trees {
		fixture := filepath.Base(filepath.Dir(filepath.Dir(tree)))
		if !slices.Contains(recordable, fixture) {
			t.Errorf("%s exists but TestRecordGoldens does not record %s; delete it", tree, fixture)
		}
	}
}
