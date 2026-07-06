//go:build e2e

package e2e

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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
	{"fork_upstream", "fork_upstream", "expected/outs.json"},
	// #99: upstream MAP-fork with real keys — the O(1) element path slicing a
	// producer bundle for a map fork (driver UTF-8 key sort vs Go forkkeys).
	{"map_fork_upstream", "map_fork_upstream", "expected/outs.json"},
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
	// #99: a file-bearing LEAF scatter (split element is a file) — keeps the
	// FORK resolve (O(total)) under -native but folds its MERGE; TestNativeMode
	// reruns it with -native.
	{"map_file_split", "map_file_split", "expected/outs.json"},
	{"map_pipe_nested", "map_pipe_nested", "expected/outs.json"},
	{"map_pipe_disabled_nested", "map_pipe_disabled_nested", "expected/outs.json"},
	{"map_pipe_disabled", "map_pipe_disabled", "expected/outs.json"},
	{"map_pipe_split", "map_pipe_split", "expected/outs.json"},
	{"map_file_keyed", "map_file_keyed", "expected/outs.json"},
	{"struct_of_file", "struct_of_file", "expected/outs.json"},
	// #173: pipeline output typed as a callable's output struct with a file leaf.
	{"callable_struct_file", "callable_struct_file", "expected/outs.json"},
	// #172: array<map<Point>>.x projection fed to a map<int>[] consumer.
	{"arr_map_proj", "arr_map_proj", "expected/outs.json"},
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
	// #113: an exec stage whose src line carries arguments after the path —
	// the outputs depend on both args, so dropping -srcarg fails loudly. The
	// golden is a real mrp run (the stub speaks the journal protocol).
	{"src_args", "src_args", "expected/outs.json"},
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
	// #217: file-typed chunk-def arg the split phase creates, read by the JOIN.
	{"split_chunk_file", "split_chunk_file", "expected/outs.json"},
	// Coverage matrix: file leaves at the split/join chunk boundary.
	{"split_struct_chunk", "split_struct_chunk", "expected/outs.json"},
	{"chunk_out_filearr", "chunk_out_filearr", "expected/outs.json"},
	{"chunk_out_dir", "chunk_out_dir", "expected/outs.json"},
	{"mixed_adapters", "mixed_adapters", "expected/outs.json"},
	{"struct_file_array", "struct_file_array", "expected/outs.json"},
	{"file_tree", "file_tree", "expected/outs.json"},
	{"map_null_map", "map_null_map", "expected/outs.json"},
	{"map_key_sort", "map_key_sort", "expected/outs.json"},
	{"map_file_array", "map_file_array", "expected/outs.json"},
	// mrp-anchored goldens for the native-suite fixtures, so the -native runs
	// compare against mrp truth in ONE run instead of a second default run per
	// fixture (TestGolden here proves default==golden; native==golden follows).
	{"fork_ref", "fork_ref", "expected/outs.json"},
	{"fork_mid", "fork_mid", "expected/outs.json"},
	// #99 empty-fork fidelity: an invocation-known split source that resolves
	// EMPTY merges to null (bind.Merge emptyNull, matching mrp's static
	// resolver pruning a zero-fork call — empty_fork_min regenerated from
	// current mrp: null, not []); runtime_empty_forks pins the RUNTIME side
	// (typed empty for an upstream empty/null collection) plus the zero-fork
	// sentinel. The rule is shape-based, not value-based, so the _override
	// case proves a launch-time entry override to a NON-empty collection still
	// runs the forks — the hazard that bars baking the empty statically.
	{"empty_map_fork", "empty_map_fork", "expected/outs.json"},
	{"empty_fork_min_override", "empty_fork_min", "expected/override_outs.json"},
	{"runtime_empty_forks", "runtime_empty_forks", "expected/outs.json"},
	// #127 value-chain empty-fork fidelity — the three divergence classes the
	// old entrySplit comment admitted, closed by knownInvocation propagation:
	// a sub-pipeline split chaining to an entry value through the parent
	// call's bindings, an in-pipeline cascade `map call SECOND(split
	// FIRST.scaled)` over an invocation-known-empty FIRST, and a MIXED split
	// (entry ref zipped with an upstream ref, entry side empty). mrp prunes
	// all three to null; the _override cases prove the marking stays a
	// runtime shape rule (launch-override the entry side non-empty and the
	// forks run).
	{"empty_fork_sub", "empty_fork_sub", "expected/outs.json"},
	{"empty_fork_sub_override", "empty_fork_sub", "expected/override_outs.json"},
	{"empty_fork_cascade", "empty_fork_cascade", "expected/outs.json"},
	{"empty_fork_cascade_override", "empty_fork_cascade", "expected/override_outs.json"},
	{"empty_fork_mixed", "empty_fork_mixed", "expected/outs.json"},
	{"fork_disabled_sub", "fork_disabled_sub", "expected/outs.json"},
	{"fork_disabled_skip", "fork_disabled_skip", "expected/outs.json"},
	{"fork_fanout", "fork_fanout", "expected/outs.json"},
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
	// #76: a transform return with a file output — the native LAYOUT file-leaf path.
	{"native_file_return", "native_file_return", "expected/nf_outs.json"},
}

// TestGolden is the end-to-end differential suite (port of run.sh): transpile
// each fixture, run it under real Nextflow, and assert the published
// pipeline_outs.json matches the committed golden mrp output. Cases run in
// parallel because each is an independent `nextflow run` and JVM startup
// dominates; bound the pool with go test's -parallel flag.
func TestGolden(t *testing.T) {
	t.Parallel()

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

// goldenStringLeaves returns every string leaf of the golden outs JSON, in
// walk order (arrays in order, map values unordered).
func goldenStringLeaves(t *testing.T, goldenPath string) []string {
	t.Helper()

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}

	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("parse golden %s: %v", goldenPath, err)
	}

	var leaves []string

	var walk func(v any)
	walk = func(v any) {
		switch tv := v.(type) {
		case string:
			leaves = append(leaves, tv)
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

	return leaves
}

// assertPublishedLeaves walks the golden outs JSON and requires every string
// leaf to exist on disk under resultsDir — verifying physical outs/ placement,
// not just the rel strings goldenJSON diffs. Only usable for fixtures whose
// string leaves are all published file paths (file_tree is; a fixture with a
// plain string output is not).
func assertPublishedLeaves(t *testing.T, resultsDir, goldenPath string) {
	t.Helper()

	for _, leaf := range goldenStringLeaves(t, goldenPath) {
		if _, err := os.Stat(filepath.Join(resultsDir, leaf)); err != nil {
			t.Errorf("published leaf %s not on disk: %v", leaf, err)
		}
	}
}

// assertPublishedTreeExact strengthens assertPublishedLeaves from subset-
// existence to set EQUALITY: after the leaf-existence walk, the full file set
// under resultsDir must equal the golden's string-leaf set, so a LAYOUT bug
// publishing stray extra files fails, not only a missing leaf. The Nextflow-
// side metadata (pipeline_outs.json, manifest.json.gz) is excluded, mirroring
// compareMrpToNextflow. Only usable when every string leaf is a published
// file path and no output is directory-typed (a dir leaf's inner files are
// not listed in the golden).
func assertPublishedTreeExact(t *testing.T, resultsDir, goldenPath string) {
	t.Helper()

	assertPublishedLeaves(t, resultsDir, goldenPath)

	want := slices.Compact(slices.Sorted(slices.Values(goldenStringLeaves(t, goldenPath))))
	got := slices.Sorted(maps.Keys(hashTree(t, resultsDir, "pipeline_outs.json", "manifest.json.gz")))

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("published results/ file set != golden leaf set (-want +got):\n%s", diff)
	}
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

// TestEntryFileAdversarialName pins the EMITTER side of the -fileflat JSON
// seam: BUILD_ENTRY_ARGS renders the staged-path map with
// groovy.json.JsonOutput.toJson inside a quoted heredoc, and that encoding —
// not just mre's decoder, which TestReadFlatFile covers — must keep a legal
// filename containing every separator the old flat encoding used (`,`, `;`,
// `=`) intact. The adversarial copy of entry_file's override input is created
// at runtime (committing such a basename would be fragile across tooling); the
// stage sums the file's numbers, so the run reproduces the override golden
// (total = 200) only if the path survived the emit -> entryargs seam — the old
// encoding mis-split it at `;` and `,`.
func TestEntryFileAdversarialName(t *testing.T) {
	requireTools(t, "nextflow", "java")

	fixtureDir := filepath.Join(root, "testdata", "entry_file")
	proj := transpile(t, "entry_file")

	content, err := os.ReadFile(filepath.Join(fixtureDir, "input", "override.txt"))
	if err != nil {
		t.Fatalf("read override input: %v", err)
	}

	advPath := filepath.Join(t.TempDir(), "a,b;c=d.txt")
	if err := os.WriteFile(advPath, content, 0o644); err != nil {
		t.Fatalf("write adversarial input: %v", err)
	}

	params, err := json.Marshal(map[string]string{"reads": advPath})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	if err := os.WriteFile(filepath.Join(proj, "params.json"), params, 0o644); err != nil {
		t.Fatalf("write params.json: %v", err)
	}

	if err := runNextflow(t, proj, "-params-file", "params.json"); err != nil {
		t.Fatal(err)
	}

	goldenJSON(t,
		filepath.Join(proj, "results", "pipeline_outs.json"),
		filepath.Join(fixtureDir, "expected", "ep_override_outs.json"))
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

// TestNativeMode guards the -native shapes that do NOT fully collapse: each
// case pins the EXACT surviving data-plane inventory — every process name in
// the planeCategories (BIND/FORK/MERGE/DISABLE/BUILD_ENTRY_ARGS), compared as
// a full set — so presence, keyed variant (FORK_x vs FORK_x_K vs FORK_x_KS),
// and cardinality are all enforced; a new or vanished task fails loudly. #76
// M1 step 1 (no BUILD_ENTRY_ARGS task — entry args are baked at transpile
// time) is enforced by that category never appearing in any plane list.
// plan.go kind routing: a kindMapped call keeps ONE FORK resolve, folding its
// MERGE into a sole consumer; a disabled mapped call keeps its MERGE as the
// skip-branch mix point; a keyed sub-pipeline body keeps its inner data plane
// as keyed (_K) or #110 driver-scatter (_KS) variants. Every run must match
// the committed mrp golden: TestGolden proves default==golden for these
// fixtures, so native==golden covers native==default transitively in half the
// runs. File- and directory-typed entries are covered by TestNativeFileEntry;
// the fully-collapsed fixtures live in TestNativeComplete.
func TestNativeMode(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	cases := []struct {
		fixture, golden string
		plane           []string // exact surviving data-plane process names
	}{
		{"chain_fuse3", "expected/outs.json", nil},
		// cellranger_shaped's PER_SAMPLE map call is not element-scatterable, so
		// exactly ONE FORK resolve remains (the gather folds into the sole
		// consumer); two fan-in calls keep plain BIND tasks.
		{"cellranger_shaped", "expected/count_outs.json", []string{
			"BIND_13_BASIC_COUNTER__POST_MATRIX",
			"BIND_5_COUNT__BASIC_COUNTER",
			"FORK_11_POST_MATRIX__PER_SAMPLE",
		}},
		// #99 kindMapped shapes under -native: a file-bearing leaf scatter, a
		// multi-split leaf, and a split-stage callee (#106) each keep exactly ONE
		// FORK resolve and fold their MERGE. All must match the mrp golden.
		{"map_file_split", "expected/outs.json", []string{"FORK_3_FBS__PROC"}},
		{"multisplit", "expected/outs.json", []string{"FORK_2_MS__PAIR"}},
		{"map_split", "expected/outs.json", []string{"FORK_2_MS__SUMSQ"}},
		// #121 map-over-sub-pipeline family (the keyed layer #107/#110 landed
		// on). A sub-pipeline callee is kindMapped — ONE outer FORK resolve,
		// MERGE folded into the sole consumer; leaf calls inside the keyed body
		// fuse (#107), so map_pipe keeps nothing else.
		{"map_pipe", "expected/outs.json", []string{"FORK_5_OUTER__INNER"}},
		// map_pipe_nested's value-only inner map rides the #110 driver scatter:
		// a single FORK_..._KS (no per-outer-fork FORK_K resolve) feeding ONE
		// keyed MERGE_K gather; INNER's merged return keeps its BIND in both the
		// plain and keyed module bodies.
		{"map_pipe_nested", "expected/outs.json", []string{
			"BIND_5_INNER__return",
			"BIND_5_INNER__return_K",
			"FORK_5_INNER__DBL_KS",
			"FORK_5_OUTER__INNER",
			"MERGE_5_INNER__DBL_K",
		}},
		// A split-stage call inside the keyed body keeps only its keyed BIND —
		// the callee runs the keyed split triad, no inner FORK or MERGE.
		{"map_pipe_split", "expected/outs.json", []string{
			"BIND_5_INNER__SUMSQ_K",
			"FORK_5_OUTER__INNER",
		}},
		// map_pipe_disabled's disable sits on the INNER keyed leaf call DBL
		// (`disabled = self.skip` inside INNER's body), NOT on the outer mapped
		// sub-pipeline call — so the outer MERGE still folds and the gate
		// survives as DBL's keyed DISABLE task plus its keyed BIND.
		{"map_pipe_disabled", "expected/outs.json", []string{
			"BIND_5_INNER__DBL_K",
			"BIND_5_INNER__return",
			"BIND_5_INNER__return_K",
			"DISABLE_5_INNER__DBL_K",
			"FORK_5_OUTER__INNER",
		}},
		// map_pipe_disabled_nested's DISABLED inner map is never scatterable
		// (keyedScatterable rejects c.Disabled), so the keyed body keeps the
		// per-outer-fork FORK_K resolve and its MERGE_K skip-branch mix point —
		// and the plain INNER body (emitted alongside, though only the keyed
		// one is invoked) keeps the plain FORK/MERGE pair for the same reason.
		{"map_pipe_disabled_nested", "expected/outs.json", []string{
			"BIND_5_INNER__return",
			"BIND_5_INNER__return_K",
			"FORK_5_INNER__DBL",
			"FORK_5_INNER__DBL_K",
			"FORK_5_OUTER__INNER",
			"MERGE_5_INNER__DBL",
			"MERGE_5_INNER__DBL_K",
		}},
		// A DISABLED plain-context map call is never scatterable
		// (nativeScatterable rejects c.Disabled) — kindMapped keeps its ONE
		// FORK, and the foldMerge pass skips disabled calls, so the ONE MERGE
		// remains as the skip-branch mix point.
		{"disabled_map", "expected/outs.json", []string{
			"FORK_1_Q__DBL",
			"MERGE_1_Q__DBL",
		}},
		{"disabled_map_ref", "expected/outs.json", []string{
			"FORK_1_P__DBL",
			"MERGE_1_P__DBL",
		}},
		// dead_map_pipe's map call sits in an UNREACHABLE pipeline (#59): the
		// live path collapses fully; the dead module keeps only its return BIND.
		// That full collapse is coupled to the dead map being value-only
		// scatterable (kindNativeScatter): keyedReachable roots EVERY declared
		// pipeline, so a file-bearing, multi-split, split-stage, or
		// sub-pipeline dead map would stay kindMapped and emit the callee's
		// keyed variants despite never running. kitchen_sink's fan-in calls
		// keep plain BIND tasks but no fork machinery. Neither is BIND-free, so
		// they cannot join TestNativeComplete.
		{"dead_map_pipe", "expected/outs.json", []string{"BIND_4_DEAD__return"}},
		{"kitchen_sink", "expected/main_outs.json", []string{
			"BIND_4_MAIN__SCALE_ALL",
			"BIND_4_MAIN__SCALE_ALL2",
			"BIND_9_SCALE_ALL__return",
		}},
		// #127 value-chain emptyNull under -native: a sub-pipeline split fed an
		// entry value through the parent's bindings scatters in-workflow inside
		// the sub-pipeline (boundary BIND + its return BIND remain), and a
		// MIXED entry+upstream zip is multi-split, so it keeps its ONE FORK
		// resolve. Both must merge the zero-fork run to null, matching mrp's
		// static prune.
		{"empty_fork_sub", "expected/outs.json", []string{
			"BIND_3_EFS__SUB",
			"BIND_3_SUB__return",
		}},
		{"empty_fork_mixed", "expected/outs.json", []string{"FORK_3_EFM__PAIR"}},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, "-native")
			assertPlaneProcesses(t, proj, tc.plane)

			if err := runNextflow(t, proj); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.fixture, tc.golden))
		})
	}
}

// TestNativeCombos guards -native composed with the other plan levers (#122):
// buildPlan folds -fuse-chains' kindFusedChain and -fold-disables'
// kindFoldedOff into the same plan -native scatters, but nothing exercised the
// compositions. Each case pins the exact surviving data-plane inventory
// (empty: these linear value shapes collapse fully) and must reproduce the
// committed mrp golden — but plane emptiness and golden output alone cannot
// distinguish "composed" from "lever silently ignored", since each fixture
// also collapses and matches under plain -native. So each case additionally
// asserts the lever fired in the generated module: a fused chain runs as ONE
// process staging every constituent's bind spec (specs counts them, absent
// names the folded-away standalone processes), and a folded-off gate leaves
// only its null channel (fired) with no stage process (absent), mirroring
// TestFuseChains/TestFoldDisables' single-flag structural checks.
func TestNativeCombos(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	cases := []struct {
		fixture string
		flags   []string
		golden  string
		module  string   // pipeline module the lever rewrites
		specs   int      // fused-chain spec inputs the surviving process stages (0: not a fusion case)
		fired   string   // substring proving the lever fired (fusion cases use specs instead)
		absent  []string // declarations the lever must have removed
	}{
		{
			fixture: "chain_fuse",
			flags:   []string{"-native", "-fuse-chains"},
			golden:  "expected/outs.json",
			module:  "pipe_CH.nf",
			specs:   2,
			absent:  []string{"process STAGE_2_CH__SRC"},
		},
		{
			fixture: "chain_fuse3",
			flags:   []string{"-native", "-fuse-chains"},
			golden:  "expected/outs.json",
			module:  "pipe_P.nf",
			specs:   3,
			absent:  []string{"process STAGE_1_P__A", "process STAGE_1_P__B"},
		},
		{
			fixture: "fold_disable",
			flags:   []string{"-native", "-fold-disables"},
			golden:  "expected/outs.json",
			module:  "pipe_P.nf",
			fired:   "ch_GEN = Channel.value(",
			absent:  []string{"process STAGE_1_P__GEN"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, tc.flags...)
			assertPlaneProcesses(t, proj, nil)

			mod, err := os.ReadFile(filepath.Join(proj, "modules", tc.module))
			if err != nil {
				t.Fatal(err)
			}

			if tc.specs > 0 {
				if got := strings.Count(string(mod), "path 'spec_"); got != tc.specs {
					t.Errorf("%v: want one fused process staging %d specs, got %d spec inputs:\n%s",
						tc.flags, tc.specs, got, mod)
				}
			}

			if tc.fired != "" && !strings.Contains(string(mod), tc.fired) {
				t.Errorf("%v: lever did not fire, missing %q:\n%s", tc.flags, tc.fired, mod)
			}

			for _, a := range tc.absent {
				if strings.Contains(string(mod), a) {
					t.Errorf("%v: lever left %q in the module:\n%s", tc.flags, a, mod)
				}
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

// TestNativeFileEntry verifies -native supports file- AND directory-typed entry
// inputs (#99): the entry args (including their file/dir leaves) are baked at
// transpile time and staged into the workflow, so each fixture's -native run
// reproduces its mrp golden (the baked default invocation).
func TestNativeFileEntry(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	// leaves gates the #120 physical publish verification exactly as
	// TestNativeComplete's flag does. All five goldens are scalar totals today
	// (the fixtures CONSUME file entries and publish no files), so none opts
	// in: an unconditional walk would false-fail os.Stat on a future scalar
	// STRING output, and a future file-typed out must consciously flip its
	// flag to get the exact published-tree check.
	cases := []struct {
		fixture, golden string
		leaves          bool
	}{
		{fixture: "entry_file", golden: "expected/ep_outs.json"},
		{fixture: "entry_filearr", golden: "expected/epa_outs.json"},
		{fixture: "entry_struct_file", golden: "expected/eps_outs.json"},
		{fixture: "entry_mapfile", golden: "expected/epm_outs.json"},
		{fixture: "entry_dir", golden: "expected/epd_outs.json"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture, "-native")
			if err := runNextflow(t, proj); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.fixture, tc.golden))

			if tc.leaves {
				assertPublishedTreeExact(t, filepath.Join(proj, "results"),
					filepath.Join(root, "testdata", tc.fixture, tc.golden))
			}
		})
	}
}

// TestNativeRejectsOverride guards the #103 override guard: -native bakes entry
// args at transpile time, so a launch-time param naming a baked input must be a
// loud error, not a silently-ignored override (which would diverge from mrp).
func TestNativeRejectsOverride(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "entry_file", "-native")
	if err := runNextflow(t, proj, "--reads", "/tmp/whatever.txt"); err == nil {
		t.Error("-native must reject a launch-time override of a baked entry arg")
	}
}

// TestNativeContainerEmits verifies -native + -native-runner transpile cleanly
// for a container target (#99): the project builds with the runner baked into
// the image at ctrRunner and the generated scripts referencing that in-image
// path (no mounted project dir). TestGeneratedAWSBatchImageNative runs the
// built image end to end; here we assert the transpile succeeds and wires the
// baked paths.
func TestNativeContainerEmits(t *testing.T) {
	buildBinaries(t)

	proj := t.TempDir()
	dir := filepath.Join(root, "testdata", "diamond_min")
	cmd := exec.Command(filepath.Join(root, "mro2nf"),
		"-native", "-native-runner", "-target", "awsbatch", "-container", "example/img:1",
		"-o", proj, "-mre", filepath.Join(root, "mre"),
		"-shell", filepath.Join(root, "vendor-martian", "python", "martian_shell.py"),
		"-mropath", dir, filepath.Join(dir, "pipeline.mro"))
	cmd.Dir = root

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("-native container transpile must succeed; got:\n%s", out)
	}

	dockerfile, err := os.ReadFile(filepath.Join(proj, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "/opt/mro2nf/runner") {
		t.Error("container Dockerfile must bake the -native-runner runner")
	}

	main, err := os.ReadFile(filepath.Join(proj, "modules", "pipe_D.nf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "/opt/mro2nf/runner/run_stage.py") {
		t.Error("container native-runner scripts must exec the baked runner path")
	}
}

// planeCategories are the data-plane process-name prefixes -native aims to
// collapse: TestNativeMode pins the exact survivors per fixture and
// assertNativeComplete requires none at all.
var planeCategories = []string{"BIND", "FORK", "MERGE", "DISABLE", "BUILD_ENTRY_ARGS"}

// processDecl matches a Nextflow process declaration and captures its name.
var processDecl = regexp.MustCompile(`(?m)^process +(\w+)`)

// planeProcesses returns the sorted names of every process declared in the
// generated project (modules + main.nf) whose name starts with one of the
// planeCategories prefixes.
func planeProcesses(t *testing.T, proj string) []string {
	t.Helper()

	mods, err := filepath.Glob(filepath.Join(proj, "modules", "*.nf"))
	if err != nil {
		t.Fatal(err)
	}

	var procs []string

	for _, f := range append(mods, filepath.Join(proj, "main.nf")) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range processDecl.FindAllStringSubmatch(string(data), -1) {
			if slices.ContainsFunc(planeCategories, func(cat string) bool {
				return strings.HasPrefix(m[1], cat)
			}) {
				procs = append(procs, m[1])
			}
		}
	}

	slices.Sort(procs)

	return procs
}

// assertPlaneProcesses fails unless the generated data-plane inventory equals
// want exactly — enforcing presence, keyed variant (FORK_x vs FORK_x_K vs
// FORK_x_KS), and cardinality, not mere substring occurrence.
func assertPlaneProcesses(t *testing.T, proj string, want []string) {
	t.Helper()

	sorted := slices.Sorted(slices.Values(want))

	if diff := cmp.Diff(sorted, planeProcesses(t, proj)); diff != "" {
		t.Errorf("data-plane processes mismatch (-want +got):\n%s", diff)
	}
}

// assertNoProcesses fails if any process in the project's generated Nextflow
// (modules + main.nf) matches one of the given `process <CAT>` prefixes.
func assertNoProcesses(t *testing.T, proj string, cats ...string) {
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

		for _, cat := range cats {
			if strings.Contains(string(data), "process "+cat) {
				t.Errorf("%s: -native must not emit a %q task", filepath.Base(f), cat)
			}
		}
	}
}

// assertNativeComplete fails if any BIND/FORK/MERGE/DISABLE/BUILD_ENTRY_ARGS
// process is emitted — the #76 acceptance that those data-plane task categories
// vanish under -native. (#76 also names STRUCTIFY, but no STRUCTIFY process
// exists anywhere in the emitter — struct outputs flow through ordinary
// bundles — so there is nothing to assert against.)
func assertNativeComplete(t *testing.T, proj string) {
	t.Helper()
	assertNoProcesses(t, proj, planeCategories...)
}

// TestNativeScatter guards the #76 native-map shapes that are NOT fully
// task-free but must still scatter with no FORK task, byte-identical to the
// default run. Each case states its MERGE contract: fork_disabled_sub (scatter
// behind a disabled sub-pipeline's QUEUE pipeargs, every fork must run; its
// merge folds, so no MERGE — the enclosing TOP keeps BIND tasks),
// fork_disabled_skip (the same shape with skip=true baked: zero instances run
// and the folded consumer must stay DORMANT, yielding the null result), and
// fork_fanout (two consumers, so the MERGE task must REMAIN rather than fold).
// The fully-collapsed fixtures live in TestNativeComplete.
func TestNativeScatter(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	cases := []struct {
		fixture   string
		wantMerge bool
	}{
		{"fork_disabled_sub", false},
		{"fork_disabled_skip", false},
		{"fork_fanout", true},
		// #127 cascade under -native: SECOND scatters over FIRST.scaled with
		// emptyNull set by the knownInvocation cascade — the zero-width
		// upstream scatter runs its keys-only sentinel and merges to null.
		// FIRST keeps its MERGE (two consumers: SECOND's split + the return).
		{"empty_fork_cascade", true},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			native := transpile(t, tc.fixture, "-native")
			assertNoProcesses(t, native, "FORK")

			if tc.wantMerge {
				assertHasProcess(t, native, "MERGE")
			} else {
				assertNoProcesses(t, native, "MERGE")
			}

			if err := runNextflow(t, native); err != nil {
				t.Fatal(err)
			}

			// Compared against the committed mrp golden, not a fresh default run:
			// TestGolden already proves default==golden for these fixtures, so
			// native==golden covers native==default transitively in half the runs
			// — and anchors native to mrp truth rather than to whatever the
			// default path happens to produce.
			goldenJSON(t,
				filepath.Join(native, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.fixture, "expected", "outs.json"))
		})
	}
}

// assertHasProcess fails unless some generated file declares a process with
// the given prefix — guarding that a deliberately-kept task really remains.
func assertHasProcess(t *testing.T, proj string, cat string) {
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

		if strings.Contains(string(data), "process "+cat) {
			return
		}
	}

	t.Errorf("expected a %q process in the generated project", cat)
}

// TestNativeComplete guards #76 for the subset that -native fully collapses:
// no data-plane task categories remain, and the output is byte-identical to
// the default (bundle-mode) run. file_min exercises a file output through the
// native LAYOUT's inline return bind. native_file_return has a TRANSFORM return
// with a file output, so it exercises the native LAYOUT's file-leaf path (the
// inline bind's args/f -> leaves -> PUBLISH_LEAF); file_min/chain_fuse/struct_min
// are forward returns (default LAYOUT) that only drop BUILD_ENTRY_ARGS. The
// map-call fixtures collapse the whole fork machinery: the scatter kills FORK
// and the sole-consumer merge fold kills MERGE — fork_min (array), map_fork
// (typed map), and fork_ref (an upstream broadcast ref re-read per instance).
// empty_fork_min/empty_map_fork are INVOCATION-known empty forks: the scatter
// still runs its keys-only sentinel, and the zero-fork merge emits null
// (bind.Merge emptyNull — matching mrp's static resolver, while keeping entry
// inputs launch-overridable). fork_upstream and map_null_map scatter over an
// UPSTREAM split source (#99): the driver slices the producer's value-channel
// bundle into per-fork elements (Mro2nf.forkElements); map_null_map and
// runtime_empty_forks exercise the RUNTIME zero-fork keys-only sentinel on
// that path (typed-empty results across null/empty x array/map).
func TestNativeComplete(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	// Each case compares its single -native run against the committed mrp
	// golden: TestGolden proves default==golden, so native==golden covers
	// native==default transitively — and anchors native to mrp truth.
	//
	// leaves marks the file-bearing fixtures (#120): the published results/
	// file set must EQUAL the golden's path-leaf set — a native LAYOUT bug
	// publishing correct rel paths to the wrong place (or nowhere) fails the
	// existence walk, and one publishing STRAY extra files fails the set
	// comparison. note additionally pins the published note.txt's bytes, so
	// the leaf carries the right CONTENT, not merely a file.
	cases := []struct {
		fixture, golden string
		leaves          bool
		note            string
	}{
		{fixture: "diamond_min", golden: "expected/outs.json"},
		{fixture: "fold_disable", golden: "expected/outs.json"},
		{fixture: "native_file_return", golden: "expected/nf_outs.json", leaves: true, note: "x=21"},
		{fixture: "file_min", golden: "expected/outs.json", leaves: true, note: "x=42"},
		{fixture: "chain_fuse", golden: "expected/outs.json"},
		{fixture: "struct_min", golden: "expected/stats_pipe_outs.json"},
		{fixture: "fork_min", golden: "expected/scale_all_outs.json"},
		{fixture: "map_fork", golden: "expected/outs.json"},
		{fixture: "empty_fork_min", golden: "expected/outs.json"},
		{fixture: "empty_map_fork", golden: "expected/outs.json"},
		{fixture: "fork_ref", golden: "expected/outs.json"},
		{fixture: "fork_mid", golden: "expected/outs.json"},
		{fixture: "fork_upstream", golden: "expected/outs.json"},
		{fixture: "map_fork_upstream", golden: "expected/outs.json"},
		{fixture: "map_null_map", golden: "expected/outs.json"},
		{fixture: "map_key_sort", golden: "expected/outs.json"},
		{fixture: "runtime_empty_forks", golden: "expected/outs.json"},
		// #121: value-only leaf scatters whose outputs (not split elements)
		// carry files — file arrays, keyed typed maps, struct projections, and
		// multi-dimensional forks all ride the O(1) element path with the merge
		// folded into the sole consumer.
		{fixture: "map_file", golden: "expected/outs.json", leaves: true},
		{fixture: "map_file_keyed", golden: "expected/outs.json", leaves: true},
		{fixture: "map_file_array", golden: "expected/outs.json", leaves: true},
		{fixture: "map_struct_proj", golden: "expected/outs.json"},
		{fixture: "multidim", golden: "expected/outs.json"},
		// #121: non-map shapes where every call fuses (wildcard bindings,
		// aliased calls, exec/comp adapters, a compiled split stage) — -native
		// must leave no data-plane task at all. mixed_adapters and comp_split
		// ship an mrjob.sh, which transpile() passes via -mrjob automatically.
		{fixture: "wildcard", golden: "expected/outs.json"},
		{fixture: "alias_min", golden: "expected/p_outs.json"},
		{fixture: "mixed_adapters", golden: "expected/outs.json"},
		{fixture: "comp_split", golden: "expected/outs.json"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			native := transpile(t, tc.fixture, "-native")
			assertNativeComplete(t, native)

			if err := runNextflow(t, native); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t,
				filepath.Join(native, "results", "pipeline_outs.json"),
				filepath.Join(root, "testdata", tc.fixture, tc.golden))

			if tc.leaves {
				assertPublishedTreeExact(t, filepath.Join(native, "results"),
					filepath.Join(root, "testdata", tc.fixture, tc.golden))
			}

			if tc.note != "" {
				assertFileContent(t, filepath.Join(native, "results", "note.txt"), tc.note)
			}
		})
	}
}
