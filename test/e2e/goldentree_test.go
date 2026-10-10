//go:build e2e

package e2e

// Published-tree goldens (#256): testdata/<fixture>/expected/outs/ holds the
// file tree real mrp published for the fixture's invocation, and
// expected/outs.emptydirs its empty directories, so the golden suites check
// published contents and the published file set, not only the path strings in
// pipeline_outs.json. TestRecordGoldens is the only writer.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// goldenTreeDir is where, relative to the fixture dir, the mrp-published tree
// lives. A fixture without one published no file under mrp.
const goldenTreeDir = "expected/outs"

// goldenEmptyDirs lists, one slash-separated path per line relative to
// goldenTreeDir, the empty directories mrp published. Git cannot commit an
// empty directory, so the recorder writes them here instead of into the tree.
const goldenEmptyDirs = "expected/outs.emptydirs"

const launchOverride = "launch-time -params-file override: mrp has no equivalent invocation to record"

// jsonOnlyGoldens are the goldenCases entries whose fixture mrp runs but whose
// case it cannot record, keyed by case name with the reason. They, and the
// cases of mrpUnrunnable fixtures, have no mrp-recorded tree and are checked
// on pipeline_outs.json alone.
var jsonOnlyGoldens = map[string]string{
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
		if jsonOnlyGolden(tc.name, tc.fixture) || slices.Contains(fixtures, tc.fixture) {
			continue
		}

		fixtures = append(fixtures, tc.fixture)
	}

	return fixtures
}

// jsonOnlyGolden reports whether golden case name, run from fixture, has no
// mrp-recorded tree.
func jsonOnlyGolden(name, fixture string) bool {
	_, override := jsonOnlyGoldens[name]
	_, unrunnable := mrpUnrunnable[fixture]

	return override || unrunnable
}

// assertGoldenTree requires a run's published results/ tree to equal the
// mrp-recorded expected/outs/ of golden case name: no file missing, none
// extra, every file's sha256 equal, and the same empty directories.
func assertGoldenTree(t *testing.T, name, fixture, proj string) {
	t.Helper()

	if jsonOnlyGolden(name, fixture) {
		return
	}

	want := goldenTree(t, filepath.Join(root, "testdata", fixture))
	got := hashTree(t, filepath.Join(proj, "results"), nfResultsMeta...)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published results/ differs from mrp's recorded %s/%s (-mrp +nextflow):\n%s",
			fixture, goldenTreeDir, diff)
	}
}

// goldenTree is the hashTree of fixtureDir's recorded mrp tree, with the empty
// directories goldenEmptyDirs lists added back.
func goldenTree(t *testing.T, fixtureDir string) map[string]string {
	t.Helper()

	tree := hashTree(t, filepath.Join(fixtureDir, filepath.FromSlash(goldenTreeDir)))

	data, err := os.ReadFile(filepath.Join(fixtureDir, filepath.FromSlash(goldenEmptyDirs)))
	if errors.Is(err, fs.ErrNotExist) {
		return tree
	}

	if err != nil {
		t.Fatal(err)
	}

	for rel := range strings.Lines(string(data)) {
		tree[filepath.FromSlash(strings.TrimSuffix(rel, "\n"))+"/"] = emptyDirMark
	}

	return tree
}

// TestRecordGoldens rewrites every recordable fixture's expected/outs/ and
// expected/outs.emptydirs from a real mrp run (make record-goldens). It
// replaces both wholesale, and writes neither for what mrp did not publish.
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

			if err := recordTree(filepath.Join(tmp, "mrp", "outs"), filepath.Join(root, "testdata", fixture)); err != nil {
				t.Fatalf("record %s: %v", fixture, err)
			}
		})
	}
}

// recordTree replaces fixtureDir's goldenTreeDir with copies of the files
// under mrp's outs dir src, minus mrp's root metadata, resolving symlinks to
// their contents, and its goldenEmptyDirs with src's empty directories.
func recordTree(src, fixtureDir string) error {
	dst := filepath.Join(fixtureDir, filepath.FromSlash(goldenTreeDir))
	manifest := filepath.Join(fixtureDir, filepath.FromSlash(goldenEmptyDirs))

	for _, stale := range []string{dst, manifest} {
		if err := os.RemoveAll(stale); err != nil {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	var empty []string

	err := walkPublished(src, mrpOutsMeta, func(rel, path string, emptyDir bool) error {
		if emptyDir {
			empty = append(empty, filepath.ToSlash(rel)+"\n")

			return nil
		}

		return recordFile(path, filepath.Join(dst, rel))
	})
	if err != nil || len(empty) == 0 {
		return err
	}

	slices.Sort(empty)

	if err := os.WriteFile(manifest, []byte(strings.Join(empty, "")), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", manifest, err)
	}

	return nil
}

// recordFile copies the contents of the published file src to out.
func recordFile(src, out string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", out, err)
	}

	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}

	return nil
}

// TestGoldenTreeTables fails on a jsonOnlyGoldens entry naming no golden case,
// and on an expected/outs/ tree or expected/outs.emptydirs TestRecordGoldens
// does not write (one the recorder cannot reproduce from mrp).
func TestGoldenTreeTables(t *testing.T) {
	t.Parallel()

	for name := range jsonOnlyGoldens {
		if !slices.ContainsFunc(goldenCases, func(tc goldenCase) bool { return tc.name == name }) {
			t.Errorf("jsonOnlyGoldens lists %q, which is not a goldenCases entry", name)
		}
	}

	var trees []string

	for _, golden := range []string{goldenTreeDir, goldenEmptyDirs} {
		matches, err := filepath.Glob(filepath.Join(root, "testdata", "*", filepath.FromSlash(golden)))
		if err != nil {
			t.Fatal(err)
		}

		trees = append(trees, matches...)
	}

	recordable := recordableFixtures()

	for _, tree := range trees {
		fixture := filepath.Base(filepath.Dir(filepath.Dir(tree)))
		if !slices.Contains(recordable, fixture) {
			t.Errorf("%s exists but TestRecordGoldens does not record %s; delete it", tree, fixture)
		}
	}
}
