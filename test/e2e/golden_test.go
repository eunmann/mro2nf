//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
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
