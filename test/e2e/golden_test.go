//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goldenCases mirrors the CASES table in run.sh: a fixture directory under
// testdata/ plus the committed mrp golden (relative to the fixture dir) the
// Nextflow run must reproduce. An *_override entry reruns its fixture with the
// file-typed entry input(s) supplied at launch via -params-file, so the run's
// output must reflect the OVERRIDE content, not the baked default.
var goldenCases = []struct {
	name    string
	fixture string
	golden  string
}{
	{"split_test", "split_test", "expected/SUM_SQUARE_PIPELINE/fork0/_outs"},
	{"fork_min", "fork_min", "expected/scale_all_outs.json"},
	{"struct_min", "struct_min", "expected/stats_pipe_outs.json"},
	{"modifiers_min", "modifiers_min", "expected/top_outs.json"},
	{"alias_min", "alias_min", "expected/p_outs.json"},
	{"exec_min", "exec_min", "expected/ep_outs.json"},
	{"kitchen_sink", "kitchen_sink", "expected/main_outs.json"},
	{"file_chain", "file_chain", "expected/cp_outs.json"},
	{"file_min", "file_min", "expected/outs.json"},
	{"diamond_min", "diamond_min", "expected/outs.json"},
	{"empty_fork_min", "empty_fork_min", "expected/outs.json"},
	{"stage_entry", "stage_entry", "expected/outs.json"},
	{"struct_proj", "struct_proj", "expected/outs.json"},
	{"map_fork", "map_fork", "expected/outs.json"},
	{"map_split", "map_split", "expected/outs.json"},
	{"map_pipe", "map_pipe", "expected/outs.json"},
	{"map_file", "map_file", "expected/outs.json"},
	{"map_pipe_nested", "map_pipe_nested", "expected/outs.json"},
	{"map_pipe_disabled_nested", "map_pipe_disabled_nested", "expected/outs.json"},
	{"map_pipe_disabled", "map_pipe_disabled", "expected/outs.json"},
	{"map_pipe_split", "map_pipe_split", "expected/outs.json"},
	{"map_file_keyed", "map_file_keyed", "expected/outs.json"},
	{"struct_of_file", "struct_of_file", "expected/outs.json"},
	{"literals_edge", "literals_edge", "expected/outs.json"},
	{"dir_out", "dir_out", "expected/outs.json"},
	{"api_smoke", "api_smoke", "expected/outs.json"},
	{"float_to_int", "float_to_int", "expected/outs.json"},
	{"disabled_map", "disabled_map", "expected/outs.json"},
	{"map_struct_proj", "map_struct_proj", "expected/outs.json"},
	{"include_test", "include_test", "expected/outs.json"},
	{"default_out", "default_out", "expected/outs.json"},
	{"wildcard", "wildcard", "expected/outs.json"},
	{"multidim", "multidim", "expected/outs.json"},
	{"typedmap_out", "typedmap_out", "expected/outs.json"},
	{"returnonly", "returnonly", "expected/outs.json"},
	{"multisplit", "multisplit", "expected/outs.json"},
	{"join_resources", "join_resources", "expected/outs.json"},
	{"file_array", "file_array", "expected/outs.json"},
	{"comp_split", "comp_split", "expected/outs.json"},
	{"entry_file", "entry_file", "expected/ep_outs.json"},
	{"entry_file_override", "entry_file", "expected/ep_override_outs.json"},
	{"entry_filearr", "entry_filearr", "expected/epa_outs.json"},
	{"entry_filearr_override", "entry_filearr", "expected/epa_override_outs.json"},
	{"entry_struct_file", "entry_struct_file", "expected/eps_outs.json"},
	{"entry_struct_file_override", "entry_struct_file", "expected/eps_override_outs.json"},
	{"entry_mapfile", "entry_mapfile", "expected/epm_outs.json"},
	{"entry_mapfile_override", "entry_mapfile", "expected/epm_override_outs.json"},
	{"entry_dir", "entry_dir", "expected/epd_outs.json"},
	{"entry_dir_override", "entry_dir", "expected/epd_override_outs.json"},
	{"split_from_file", "split_from_file", "expected/sp_outs.json"},
	{"split_from_file_override", "split_from_file", "expected/sp_override_outs.json"},
	{"special_resource", "special_resource", "expected/outs.json"},
	{"null_in", "null_in", "expected/outs.json"},
	{"disabled_callref", "disabled_callref", "expected/outs.json"},
	{"struct_input", "struct_input", "expected/outs.json"},
	{"nested_struct", "nested_struct", "expected/outs.json"},
	{"literals", "literals", "expected/outs.json"},
	{"fanin", "fanin", "expected/outs.json"},
	{"map_split_file", "map_split_file", "expected/outs.json"},
	{"mixed_adapters", "mixed_adapters", "expected/outs.json"},
	{"struct_file_array", "struct_file_array", "expected/outs.json"},
	{"file_tree", "file_tree", "expected/outs.json"},
	{"map_null_map", "map_null_map", "expected/outs.json"},
	{"map_file_array", "map_file_array", "expected/outs.json"},
	// Regression for #59: an unreachable pipeline containing a map call — its
	// keyed-variant include must resolve (verified by the nextflow-lint gate).
	{"dead_map_pipe", "dead_map_pipe", "expected/outs.json"},
	// Regression for #59 Lever 2: a map call disabled on an UPSTREAM output ref
	// (FLAG.on), gated natively from the upstream channel (no DISABLE task).
	{"disabled_map_ref", "disabled_map_ref", "expected/outs.json"},
	// #59 Lever 4 baseline: this chain fixture must be byte-identical with the
	// default (no -fuse-chains); TestFuseChains reruns it with the flag on.
	{"chain_fuse", "chain_fuse", "expected/outs.json"},
	// #59 Lever 4 fold-safety: SRC has a second consumer via disabled = SRC.flag,
	// so it must NOT fold under -fuse-chains; TestFuseChains asserts that + reruns.
	{"chain_fuse_disable", "chain_fuse_disable", "expected/outs.json"},
	// #59 Lever 1 baseline: entry bakes skip=true so GEN is always disabled; the
	// default (gated) run must match the golden; TestFoldDisables reruns it with
	// -fold-disables (GEN pruned) and asserts the same output.
	{"fold_disable", "fold_disable", "expected/outs.json"},
	// #81 baseline: a 3-stage linear chain A->B->C; default runs three tasks,
	// TestFuseChains reruns it with -fuse-chains (all fold into one) — same output.
	{"chain_fuse3", "chain_fuse3", "expected/outs.json"},
	// #90: a trivial-compute fixture whose DAG mirrors CellRanger's
	// _basic_sc_rna_counter shape — preflight, split, a disable fan-out gating
	// aliased calls, a mapped per-sample stage, and nested sub-pipelines.
	{"cellranger_shaped", "cellranger_shaped", "expected/count_outs.json"},
}

// TestGolden is the end-to-end differential suite (port of run.sh): transpile
// each fixture, run it under real Nextflow, and assert the published
// pipeline_outs.json matches the committed golden mrp output. Cases run in
// parallel because each is an independent `nextflow run` and JVM startup
// dominates; bound the pool with go test's -parallel flag.
func TestGolden(t *testing.T) {
	requireTools(t, "nextflow", "java")

	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixtureDir := filepath.Join(root, "testdata", tc.fixture)
			proj := transpile(t, tc.fixture)

			var runArgs []string

			if strings.HasSuffix(tc.name, "_override") {
				renderOverrideParams(t, fixtureDir, proj)
				runArgs = append(runArgs, "-params-file", "params.json")
			}

			if err := runNextflow(t, proj, runArgs...); err != nil {
				t.Fatal(err)
			}

			// file_min additionally verifies the published file's content.
			if tc.name == "file_min" {
				assertFileContent(t, filepath.Join(proj, "results", "note.txt"), "x=42")
			}

			goldenJSON(t,
				filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(fixtureDir, tc.golden))

			// file_tree additionally verifies the PHYSICAL placement of every
			// published leaf: multi-segment rels through the layout ->
			// PUBLISH_LEAF join, which the JSON diff above cannot see.
			if tc.name == "file_tree" {
				assertPublishedLeaves(t, filepath.Join(proj, "results"),
					filepath.Join(fixtureDir, tc.golden))
			}
		})
	}
}

// TestFuseChains verifies #59 Lever 4: with -fuse-chains, a single-consumer
// source stage folds into its consumer's task, and the run stays byte-identical
// to the golden. chain_fuse's SRC folds into the STAGE_2_CH__USE process (which
// runs both stages' bind+main inline); no standalone SRC process remains.
func TestFuseChains(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "chain_fuse", "-fuse-chains")

	mod, err := os.ReadFile(filepath.Join(proj, "modules", "pipe_CH.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(mod), "spec_0.json") {
		t.Errorf("-fuse-chains did not emit the fused chain process:\n%s", mod)
	}

	if strings.Contains(string(mod), "process STAGE_2_CH__SRC") {
		t.Errorf("-fuse-chains left a standalone SRC process (not folded):\n%s", mod)
	}

	if err := runNextflow(t, proj); err != nil {
		t.Fatal(err)
	}

	goldenJSON(t,
		filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "chain_fuse", "expected", "outs.json"))

	// Fold-safety: chain_fuse_disable's SRC gates a third call via disabled =
	// SRC.flag, so SRC has two consumers and must NOT fold even with the flag on;
	// output stays byte-identical.
	dproj := transpile(t, "chain_fuse_disable", "-fuse-chains")

	dmod, err := os.ReadFile(filepath.Join(dproj, "modules", "pipe_CH.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(dmod), "spec_0.json") {
		t.Errorf("SRC must not fold: it also gates a call via disabled = SRC.flag:\n%s", dmod)
	}

	if err := runNextflow(t, dproj); err != nil {
		t.Fatal(err)
	}

	goldenJSON(t,
		filepath.Join(dproj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "chain_fuse_disable", "expected", "outs.json"))

	// #73: a source feeding a pure-FORWARD consumer folds too. file_chain's
	// MAKEFILE forwards into READFILE; with -fuse-chains MAKEFILE folds away and
	// the run stays byte-identical to the golden.
	fproj := transpile(t, "file_chain", "-fuse-chains")

	fmod, err := os.ReadFile(filepath.Join(fproj, "modules", "pipe_CP.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(fmod), "spec_0.json") {
		t.Errorf("-fuse-chains must fold MAKEFILE into the READFILE forward consumer:\n%s", fmod)
	}

	if strings.Contains(string(fmod), "process STAGE_1_CP__MAKEFILE") {
		t.Errorf("-fuse-chains left a standalone MAKEFILE process:\n%s", fmod)
	}

	if err := runNextflow(t, fproj); err != nil {
		t.Fatal(err)
	}

	goldenJSON(t,
		filepath.Join(fproj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "file_chain", "expected", "cp_outs.json"))

	// #81: a 3-stage chain A->B->C folds into one process (3 specs), byte-identical.
	nproj := transpile(t, "chain_fuse3", "-fuse-chains")

	nmod, err := os.ReadFile(filepath.Join(nproj, "modules", "pipe_P.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(string(nmod), "path 'spec_"); got != 3 {
		t.Errorf("3-stage chain: want one fused process staging 3 specs, got %d spec inputs:\n%s", got, nmod)
	}

	if err := runNextflow(t, nproj); err != nil {
		t.Fatal(err)
	}

	goldenJSON(t,
		filepath.Join(nproj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "chain_fuse3", "expected", "outs.json"))
}

// TestFoldDisables verifies #59 Lever 1: with -fold-disables, an entry-baked
// always-disabled stage (GEN, gated on self.skip with skip=true) is pruned to
// its null output — no GEN stage/bind/gate in the pipeline — and the run stays
// byte-identical to the golden (which mrp produced by skipping GEN).
func TestFoldDisables(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "fold_disable", "-fold-disables")

	mod, err := os.ReadFile(filepath.Join(proj, "modules", "pipe_P.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(mod), "ch_GEN = Channel.value(") {
		t.Errorf("-fold-disables must prune GEN to its null output:\n%s", mod)
	}

	// No stage/bind process definition for GEN remains (the return bind still
	// consumes ch_GEN's null, which is correct).
	if strings.Contains(string(mod), "_P__GEN {") {
		t.Errorf("-fold-disables left a GEN process definition in the pipeline:\n%s", mod)
	}

	if err := runNextflow(t, proj); err != nil {
		t.Fatal(err)
	}

	goldenJSON(t,
		filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(root, "testdata", "fold_disable", "expected", "outs.json"))
}

// assertPublishedLeaves walks the golden outs JSON and requires every string
// leaf to exist on disk under resultsDir — verifying physical outs/ placement,
// not just the rel strings goldenJSON diffs. Only usable for fixtures whose
// string leaves are all published file paths (file_tree is; a fixture with a
// plain string output is not).
func assertPublishedLeaves(t *testing.T, resultsDir, goldenPath string) {
	t.Helper()

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}

	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("parse golden %s: %v", goldenPath, err)
	}

	var walk func(v any)
	walk = func(v any) {
		switch tv := v.(type) {
		case string:
			if _, err := os.Stat(filepath.Join(resultsDir, tv)); err != nil {
				t.Errorf("published leaf %s not on disk: %v", tv, err)
			}
		case []any:
			for _, e := range tv {
				walk(e)
			}
		case map[string]any:
			for _, e := range tv {
				walk(e)
			}
		}
	}
	walk(tree)
}

// renderOverrideParams instantiates the fixture's override-params.json into
// proj/params.json, substituting the @DIR@ placeholder with the absolute
// fixture directory (the file inputs live inside the fixture).
func renderOverrideParams(t *testing.T, fixtureDir, proj string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(fixtureDir, "override-params.json"))
	if err != nil {
		t.Fatalf("read override params: %v", err)
	}

	rendered := strings.ReplaceAll(string(raw), "@DIR@", fixtureDir)

	if err := os.WriteFile(filepath.Join(proj, "params.json"), []byte(rendered), 0o644); err != nil {
		t.Fatalf("write params.json: %v", err)
	}
}

// assertFileContent asserts a published text file's content, ignoring a
// trailing newline (matching the shell harness's `$(cat ...)` comparison).
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if got := strings.TrimRight(string(raw), "\n"); got != want {
		t.Errorf("%s content = %q, want %q", path, got, want)
	}
}

// TestNativeMode guards #76 M1 step 1: -native bakes the entry args at transpile
// time so no BUILD_ENTRY_ARGS task is emitted, and the run output stays
// byte-identical to the golden. Exercises a simple diamond-shaped disable
// fixture, an N-stage chain, and the complex cellranger_shaped pipeline (all
// scalar-entry). A file-typed entry is gated out of this increment.
func TestNativeMode(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	cases := []struct{ fixture, golden string }{
		{"fold_disable", "expected/outs.json"},
		{"chain_fuse3", "expected/outs.json"},
		{"cellranger_shaped", "expected/count_outs.json"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, "-native")

			main, err := os.ReadFile(filepath.Join(proj, "main.nf"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(main), "BUILD_ENTRY_ARGS") {
				t.Error("-native must not emit a BUILD_ENTRY_ARGS task")
			}

			if err := runNextflow(t, proj); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.fixture, tc.golden))
		})
	}
}

// TestNativeRejectsFileEntry guards the M1 gate: a file-typed entry input is not
// yet supported by -native and must be a loud transpile error, not silent wrong
// output.
func TestNativeRejectsFileEntry(t *testing.T) {
	buildBinaries(t)

	dir := filepath.Join(root, "testdata", "entry_file")
	cmd := exec.Command(filepath.Join(root, "mro2nf"),
		"-native", "-o", t.TempDir(),
		"-mre", filepath.Join(root, "mre"),
		"-shell", filepath.Join(root, "vendor-martian", "python", "martian_shell.py"),
		"-mropath", dir, filepath.Join(dir, "pipeline.mro"))
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("-native on a file-entry pipeline must fail; got success:\n%s", out)
	}
	if !strings.Contains(string(out), "file-typed entry") {
		t.Errorf("want a file-typed-entry error, got:\n%s", out)
	}
}

// TestNativeRejectsContainer guards the M1 boundary: -native is validated for
// the local backend only, so a container target must be a loud error.
func TestNativeRejectsContainer(t *testing.T) {
	buildBinaries(t)

	dir := filepath.Join(root, "testdata", "diamond_min")
	cmd := exec.Command(filepath.Join(root, "mro2nf"),
		"-native", "-target", "awsbatch", "-container", "example/img:1",
		"-o", t.TempDir(), "-mre", filepath.Join(root, "mre"),
		"-shell", filepath.Join(root, "vendor-martian", "python", "martian_shell.py"),
		"-mropath", dir, filepath.Join(dir, "pipeline.mro"))
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("-native with a container target must fail; got success:\n%s", out)
	}
	if !strings.Contains(string(out), "container backends") {
		t.Errorf("want a container-backend error, got:\n%s", out)
	}
}

// assertNativeComplete fails if any BIND/FORK/MERGE/STRUCTIFY/BUILD_ENTRY_ARGS
// process is emitted — the #76 acceptance that those data-plane task categories
// vanish under -native.
func assertNativeComplete(t *testing.T, proj string) {
	t.Helper()

	mods, err := filepath.Glob(filepath.Join(proj, "modules", "*.nf"))
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range append(mods, filepath.Join(proj, "main.nf")) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		for _, cat := range []string{"process BIND", "process FORK", "process MERGE", "process STRUCTIFY", "process BUILD_ENTRY_ARGS"} {
			if strings.Contains(string(data), cat) {
				t.Errorf("%s: -native must not emit a %q task", filepath.Base(f), strings.TrimPrefix(cat, "process "))
			}
		}
	}
}

// TestNativeComplete guards #76 for the single-pipeline subset that -native fully
// collapses: no data-plane task categories remain, and the output is byte-identical
// to the default (bundle-mode) run. file_min exercises a file output through the
// native LAYOUT's inline return bind.
func TestNativeComplete(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	for _, fx := range []string{"diamond_min", "fold_disable", "file_min", "chain_fuse", "struct_min"} {
		t.Run(fx, func(t *testing.T) {
			t.Parallel()

			native := transpile(t, fx, "-native")
			assertNativeComplete(t, native)

			def := transpile(t, fx)
			if err := runNextflow(t, native); err != nil {
				t.Fatal(err)
			}
			if err := runNextflow(t, def); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(native, "results", "pipeline_outs.json"),
				filepath.Join(def, "results", "pipeline_outs.json"))
		})
	}
}
