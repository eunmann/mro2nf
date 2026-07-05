//go:build e2e

package e2e

// True differential test (port of mrp_diff.sh): run each fixture through REAL
// Martian (mrp) AND the transpiled Nextflow, then compare the published outs/
// tree — the set of output file paths relative to the outs dir plus their
// contents — and the (path-normalized) _outs JSON. This validates mre's
// publish layout and the whole transpile+runtime path against Martian itself,
// not a hand-written golden.
//
// Requires a local Martian build: set MARTIAN_BIN (default
// ~/workdir/martian/bin). Skips cleanly — never fails, even under
// E2E_REQUIRE=1 — when mrp is unavailable, so CI without a Martian checkout
// is unaffected.

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// martianBin resolves the local Martian bin dir and skips the test when mrp
// is absent. Unlike requireTools this NEVER hard-fails under E2E_REQUIRE=1:
// CI has no Martian checkout, so the differential suite is local-only.
func martianBin(t *testing.T) string {
	t.Helper()

	bin := os.Getenv("MARTIAN_BIN")
	if bin == "" {
		bin = filepath.Join(os.Getenv("HOME"), "workdir", "martian", "bin")
	}

	info, err := os.Stat(filepath.Join(bin, "mrp"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Skipf("mrp not found at %s/mrp (set MARTIAN_BIN=<martian>/bin)", bin)
	}

	return bin
}

// runMrp runs the real Martian pipeline runner on a fixture inside tmp,
// producing <tmp>/mrp as the pipestance dir. mrp requires a plain pipestance
// name and creates it in the cwd, so it runs from tmp; the fixture's input/
// dir (if any) is symlinked there so relative input paths in the .mro (e.g.
// "input/x.txt") resolve as they do for the transpiler's -mropath.
func runMrp(t *testing.T, bin, fixture, tmp string) {
	t.Helper()

	dir := filepath.Join(root, "testdata", fixture)
	if fileExists(filepath.Join(dir, "input")) {
		if err := os.Symlink(filepath.Join(dir, "input"), filepath.Join(tmp, "input")); err != nil {
			t.Fatalf("symlink input: %v", err)
		}
	}

	cmd := exec.Command(filepath.Join(bin, "mrp"), filepath.Join(dir, "pipeline.mro"), "mrp",
		"--jobmode=local", "--localcores=2", "--localmem=4", "--disable-ui", "--nopreflight")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "MROPATH="+dir)

	out, err := cmd.CombinedOutput()

	_ = os.WriteFile(filepath.Join(tmp, "mrp.log"), out, 0o644)

	if err != nil {
		t.Fatalf("mrp %s: %v\n%s", fixture, err, tail(out, 12))
	}
}

// hashTree maps relpath -> sha256 for every regular file under dir, skipping
// files whose basename is in exclude. An absent dir (a pipeline with no file
// outputs) yields an empty tree, matching mrp_diff.sh's tree() helper.
func hashTree(t *testing.T, dir string, exclude ...string) map[string]string {
	t.Helper()

	tree := map[string]string{}
	if !fileExists(dir) {
		return tree
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		for _, ex := range exclude {
			if d.Name() == ex {
				return nil
			}
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		sum := sha256.Sum256(data)
		tree[rel] = hex.EncodeToString(sum[:])

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	return tree
}

// stripPathPrefix normalizes Martian's _outs values: mrp writes absolute
// paths into the pipestance outs/ dir, mro2nf writes the same paths relative
// to its results/ dir, so strings are stripped of the outs-dir prefix
// recursively. (The Go port of normcmp.py.)
func stripPathPrefix(v any, prefix string) any {
	switch x := v.(type) {
	case string:
		return strings.TrimPrefix(x, prefix)
	case []any:
		for i := range x {
			x[i] = stripPathPrefix(x[i], prefix)
		}

		return x
	case map[string]any:
		for k := range x {
			x[k] = stripPathPrefix(x[k], prefix)
		}

		return x
	default:
		return v
	}
}

// TestMrpDiff runs each fixture under real mrp and under transpiled Nextflow
// and requires the published file tree and the _outs JSON to match.
func TestMrpDiff(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	// Fixtures whose top-level `call` runs standalone (no launch-time param
	// override). Each exercises a distinct output/pipeline shape.
	cases := []struct {
		name      string
		realMrjob bool
	}{
		{name: "file_min"},
		{name: "dir_out"},
		{name: "map_file"},
		{name: "map_file_keyed"},
		{name: "map_split_file"},
		{name: "struct_of_file"},
		{name: "struct_file_array"},
		{name: "diamond_min"},
		{name: "fork_min"},
		{name: "struct_min"},
		{name: "kitchen_sink"},
		{name: "file_tree"},
		{name: "map_null_map"},
		// #99: map-fork key ordering (mixed case/digit + astral + CJK-compat
		// keys) — the driver's UTF-8 byte sort must agree with mrp's key order.
		{name: "map_key_sort"},
		// #99: upstream map-fork element path (producer bundle sliced, real keys).
		{name: "map_fork_upstream"},
		// #99 empty-fork fidelity, both sides of the static/runtime split:
		// an invocation-known empty split source merges to null (mrp's static
		// resolver prunes the zero-fork call); an upstream empty/null
		// collection merges to the typed empty at runtime.
		{name: "empty_fork_min"},
		{name: "empty_map_fork"},
		{name: "runtime_empty_forks"},
		// #127 value-chain empty-fork classes: mrp's static resolver prunes
		// each to null (sub-pipeline chain to an entry value, in-pipeline
		// cascade through a mapped call, mixed entry+upstream zip) — the
		// live differential machine-checks the knownInvocation cascade.
		{name: "empty_fork_sub"},
		{name: "empty_fork_cascade"},
		{name: "empty_fork_mixed"},
		// The native-suite golden anchors: their committed expected/outs.json
		// files claim mrp provenance, so the live differential machine-checks
		// that claim — otherwise TestGolden + the native suites would prove
		// only self-consistency.
		{name: "fork_ref"},
		{name: "fork_mid"},
		{name: "fork_disabled_sub"},
		{name: "fork_disabled_skip"},
		{name: "fork_fanout"},
		{name: "map_file_array"},
		// #99: file-bearing leaf scatter — the FORK-resolve + folded-merge path.
		{name: "map_file_split"},
		// #90: CellRanger-shaped DAG (preflight, split, disable fan-out, aliasing,
		// map, nested pipelines) — all py stages, so it joins the mrp differential.
		{name: "cellranger_shaped"},
		// #113: stage src arguments (`src exec "code.py 3 hello"`) must reach
		// the stage under both runners. Unlike the journal-less exec stubs
		// below, this one writes journal entries (martian_shell.py
		// update_journal), so real mrp completes it — machine-checking the
		// committed golden's mrp provenance.
		{name: "src_args"},
		// TODO: the comp/exec-adapter fixtures cannot join an mrp
		// differential — their stage binaries are fake Python stand-ins for
		// mre's simpler wrapped-adapter contract, and REAL mrp hangs
		// indefinitely waiting on them (observed: mixed_adapters stuck 23+
		// minutes at DBL.fork0.chnk0.main before being killed; the hang is
		// on the mrp side of the diff, not mre's). Exercising the
		// real-mrjob-under-mre seam needs a dedicated harness that runs
		// ONLY the Nextflow side with $MARTIAN_BIN/mrjob (via the realMrjob
		// knob below) against a committed golden, not an mrp baseline.
		// {name: "comp_split", realMrjob: true},
		// {name: "mixed_adapters", realMrjob: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			runMrp(t, bin, tc.name, tmp)

			// Extra transpile flags land AFTER the fixture's auto-added
			// -mrjob; mro2nf's stdlib flag parsing keeps the LAST occurrence,
			// so the real mrjob overrides the fixture stub.
			var extra []string

			if tc.realMrjob {
				mrjob := filepath.Join(bin, "mrjob")
				if !fileExists(mrjob) {
					t.Skipf("mrjob not found at %s", mrjob)
				}

				extra = append(extra, "-mrjob", mrjob)
			}

			proj := transpile(t, tc.name, extra...)

			if err := runNextflow(t, proj); err != nil {
				t.Fatalf("nextflow: %v", err)
			}

			compareMrpToNextflow(t, tmp, proj)
		})
	}
}

// compareMrpToNextflow diffs the published file tree (paths + contents) and
// the path-normalized _outs JSON between an mrp pipestance at <tmp>/mrp and
// a Nextflow project's results/. manifest.json.gz is a Nextflow-only metadata
// index (like pipeline_outs.json), not a pipeline output, so it is excluded
// from the tree comparison.
func compareMrpToNextflow(t *testing.T, tmp, proj string) {
	t.Helper()

	mrpOuts := filepath.Join(tmp, "mrp", "outs")
	nfOuts := filepath.Join(proj, "results")

	mrpTree := hashTree(t, mrpOuts, "_outs", "manifest.json.gz")
	nfTree := hashTree(t, nfOuts, "pipeline_outs.json", "manifest.json.gz")

	if diff := cmp.Diff(mrpTree, nfTree); diff != "" {
		t.Errorf("published tree differs (-mrp +nextflow):\n%s", diff)
	}

	// Martian rewrites the published outs into the TOP pipeline's fork0
	// metadata (<pipestance>/<PIPELINE>/fork0/_outs); outs/ holds only the
	// file tree. Glob results come back sorted, so [0] is deterministic.
	matches, err := filepath.Glob(filepath.Join(tmp, "mrp", "*", "fork0", "_outs"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no mrp fork0 _outs (glob error: %v)", err)
	}

	var mrpJSON, nfJSON any

	readJSON(t, matches[0], &mrpJSON)
	readJSON(t, filepath.Join(nfOuts, "pipeline_outs.json"), &nfJSON)

	mrpJSON = stripPathPrefix(mrpJSON, strings.TrimSuffix(mrpOuts, "/")+"/")

	if diff := cmp.Diff(mrpJSON, nfJSON); diff != "" {
		t.Errorf("_outs json mismatch (-mrp(normalized) +nextflow):\n%s", diff)
	}
}
