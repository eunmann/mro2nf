//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type goldenCase struct {
	name    string
	fixture string
	golden  string
}

// goldenCases mirrors the CASES table in run.sh: a fixture directory under
// testdata/ plus the committed mrp golden (relative to the fixture dir) the
// Nextflow run must reproduce. An *_override entry reruns its fixture with the
// file-typed entry input(s) supplied at launch via -params-file, so the run's
// output must reflect the OVERRIDE content, not the baked default.
var goldenCases = []goldenCase{
	{"split_test", "split_test", "expected/SUM_SQUARE_PIPELINE/fork0/_outs"},
	{"fork_min", "fork_min", "expected/scale_all_outs.json"},
	{"fork_upstream", "fork_upstream", "expected/outs.json"},
	// #99: upstream MAP-fork with real keys — the O(1) element path slicing a
	// producer bundle for a map fork (driver UTF-8 key sort vs Go fork key order).
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
	// #99: a file-bearing LEAF scatter (split element is a file).
	{"map_file_split", "map_file_split", "expected/outs.json"},
	{"map_pipe_disabled_nested", "map_pipe_disabled_nested", "expected/outs.json"},
	{"map_pipe_disabled", "map_pipe_disabled", "expected/outs.json"},
	{"map_pipe_split", "map_pipe_split", "expected/outs.json"},
	{"map_file_keyed", "map_file_keyed", "expected/outs.json"},
	{"struct_of_file", "struct_of_file", "expected/outs.json"},
	// #173: pipeline output typed as a callable's output struct with a file leaf.
	{"callable_struct_file", "callable_struct_file", "expected/outs.json"},
	// #240: a whole ref to an array- and a map-forked call resolves per fork
	// (array/map of structs, file leaves inside), in a stage's _args and the outs.
	{"map_wholeref", "map_wholeref", "expected/outs.json"},
	// #172: array<map<Point>>.x projection fed to a map<int>[] consumer.
	{"arr_map_proj", "arr_map_proj", "expected/outs.json"},
	{"literals_edge", "literals_edge", "expected/outs.json"},
	{"dir_out", "dir_out", "expected/outs.json"},
	// #242: directory outputs with dotfiles and an empty subdirectory.
	{"dir_hidden", "dir_hidden", "expected/outs.json"},
	// #431: a file reached from several outputs names its first published path.
	{"shared_out_file", "shared_out_file", "expected/outs.json"},
	// #432: files below an N-D array stay unpublished, as under mrp.
	{"nested_path_unpublished", "nested_path_unpublished", "expected/outs.json"},
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
	// #243: fractional, per-chunk and join memory reach the stage unrounded.
	{"frac_mem", "frac_mem", "expected/outs.json"},
	// #370: fractional, per-chunk and join threads reach the stage unrounded.
	{"frac_threads", "frac_threads", "expected/outs.json"},
	{"file_array", "file_array", "expected/outs.json"},
	{"comp_split", "comp_split", "expected/outs.json"},
	// #113: an exec stage whose src line carries arguments after the path —
	// the outputs depend on both args, so dropping -srcarg fails loudly. The
	// golden is a real mrp run (the stub speaks the journal protocol).
	{"src_args", "src_args", "expected/outs.json"},
	// File-bearing entry inputs overridden at launch with absolute paths: the
	// default invocations' relative paths are outcome cases, as mrp opens
	// them from the stage's job dir.
	{"entry_file_override", "entry_file", "expected/ep_override_outs.json"},
	{"entry_filearr_override", "entry_filearr", "expected/epa_override_outs.json"},
	{"entry_struct_file_override", "entry_struct_file", "expected/eps_override_outs.json"},
	{"entry_mapfile_override", "entry_mapfile", "expected/epm_override_outs.json"},
	{"entry_dir_override", "entry_dir", "expected/epd_override_outs.json"},
	// #261: file-bearing entry inputs overridden with values that hold no file
	// (an empty file[] and map<file>, a struct with a null file field).
	{"entry_file_empty_override", "entry_file_empty", "expected/override_outs.json"},
	{"split_from_file_override", "split_from_file", "expected/sp_override_outs.json"},
	// An entry input's path a stage writes into a JSON output that another
	// task opens; the golden is mrp's run on the same host paths.
	{"mounted_paths_override", "mounted_paths", "expected/override_outs.json"},
	{"cli_typed", "cli_typed", "expected/outs.json"},
	{"special_resource", "special_resource", "expected/outs.json"},
	{"null_in", "null_in", "expected/outs.json"},
	{"disabled_callref", "disabled_callref", "expected/outs.json"},
	// #253: a disable gate that is null at run time leaves the call enabled.
	{"null_gate", "null_gate", "expected/outs.json"},
	// #339: a whole ref to a disabled stage / pipeline call reaches its
	// consumer's _args as null, not a struct of nulls.
	{"disabled_wholeref", "disabled_wholeref", "expected/outs.json"},
	{"struct_input", "struct_input", "expected/outs.json"},
	// #239: a struct bound to a narrower struct type keeps only its declared
	// members, in stage _args and pipeline outputs alike.
	{"struct_narrow", "struct_narrow", "expected/outs.json"},
	{"nested_struct", "nested_struct", "expected/outs.json"},
	// #366: struct values in mrp's key order, however they are built.
	{"struct_key_order", "struct_key_order", "expected/outs.json"},
	{"literals", "literals", "expected/outs.json"},
	// #238: whole-number float literals >= 1e6 stay floats for a Python stage.
	{"literals_big", "literals_big", "expected/outs.json"},
	{"fanin", "fanin", "expected/outs.json"},
	{"map_split_file", "map_split_file", "expected/outs.json"},
	// #217: file-typed chunk-def arg the split phase creates, read by the JOIN.
	{"split_chunk_file", "split_chunk_file", "expected/outs.json"},
	// Coverage matrix: file leaves at the split/join chunk boundary.
	{"split_struct_chunk", "split_struct_chunk", "expected/outs.json"},
	// #245: a whole-number float a split returns for an int chunk input
	// reaches main and join uncoerced.
	{"chunk_float_int", "chunk_float_int", "expected/outs.json"},
	{"chunk_out_filearr", "chunk_out_filearr", "expected/outs.json"},
	{"chunk_out_dir", "chunk_out_dir", "expected/outs.json"},
	// Matrix completeness: builtin `file` type; file forwarded into a split arg.
	{"file_builtin_min", "file_builtin_min", "expected/outs.json"},
	{"split_forward_file", "split_forward_file", "expected/outs.json"},
	{"mixed_adapters", "mixed_adapters", "expected/outs.json"},
	{"struct_file_array", "struct_file_array", "expected/outs.json"},
	{"file_tree", "file_tree", "expected/outs.json"},
	{"map_null_map", "map_null_map", "expected/outs.json"},
	{"map_key_sort", "map_key_sort", "expected/outs.json"},
	{"map_file_array", "map_file_array", "expected/outs.json"},
	// A map call broadcasting an upstream call's output beside its split, and
	// one whose sole consumer is a mid-pipeline stage.
	{"fork_ref", "fork_ref", "expected/outs.json"},
	{"fork_mid", "fork_mid", "expected/outs.json"},
	// #99 empty-fork fidelity: an invocation-known split source that resolves
	// EMPTY merges to null (mrp's static resolver prunes a zero-fork call:
	// null, not []); runtime_empty_forks pins the RUNTIME side (typed empty
	// for an upstream empty/null collection) plus the zero-fork sentinel. The
	// _override case proves a launch-time entry override to a NON-empty
	// collection still runs the forks.
	{"empty_map_fork", "empty_map_fork", "expected/outs.json"},
	{"empty_fork_min_override", "empty_fork_min", "expected/override_outs.json"},
	{"runtime_empty_forks", "runtime_empty_forks", "expected/outs.json"},
	// #127 value-chain empty-fork fidelity: a sub-pipeline split chaining to
	// an entry value through the parent call's bindings, an in-pipeline
	// cascade `map call SECOND(split FIRST.scaled)` over an
	// invocation-known-empty FIRST, and a MIXED split (entry ref zipped with an
	// upstream ref, entry side empty). mrp prunes all three to null; the
	// _override cases launch-override the entry side non-empty and the forks
	// run.
	{"empty_fork_sub", "empty_fork_sub", "expected/outs.json"},
	{"empty_fork_sub_override", "empty_fork_sub", "expected/override_outs.json"},
	{"empty_fork_cascade", "empty_fork_cascade", "expected/outs.json"},
	{"empty_fork_cascade_override", "empty_fork_cascade", "expected/override_outs.json"},
	{"empty_fork_mixed", "empty_fork_mixed", "expected/outs.json"},
	// #241: one sub-pipeline at two call sites, one invocation-known empty
	// (null) and one runtime-empty ([]); the rule is decided per call site.
	{"empty_fork_two_sites", "empty_fork_two_sites", "expected/outs.json"},
	// #383: a split over a disable-gated producer of an invocation-known
	// source (map call, mapping sub-pipeline, pass-through sub-pipeline) is
	// null under literal and runtime gates alike.
	{"empty_fork_gated", "empty_fork_gated", "expected/outs.json"},
	{"empty_fork_gated_sub", "empty_fork_gated_sub", "expected/outs.json"},
	// #435: a split of a mapped call's whole ref cascades the prune to null.
	{"empty_fork_wholeref", "empty_fork_wholeref", "expected/outs.json"},
	// #436: a split of a call an entry value disables is null, while the same
	// gate left false gives the runtime empty [].
	{"disabled_split_null", "disabled_split_null", "expected/outs.json"},
	// #434: a runtime-gated map call over a sub-pipeline is one null per fork.
	{"disabled_map_nullforks", "disabled_map_nullforks", "expected/outs.json"},
	// #433: a split through a field of a typed-map literal forks once per key.
	{"map_literal_proj", "map_literal_proj", "expected/outs.json"},
	// #433: the fork-independent calls of a map call over a sub-pipeline.
	{"fork_indep_call", "fork_indep_call", "expected/outs.json"},
	{"fork_disabled_sub", "fork_disabled_sub", "expected/outs.json"},
	{"fork_disabled_skip", "fork_disabled_skip", "expected/outs.json"},
	{"fork_fanout", "fork_fanout", "expected/outs.json"},
	// #59: a declared but never-called pipeline containing a map call.
	{"dead_map_pipe", "dead_map_pipe", "expected/outs.json"},
	// #59: a map call disabled on an UPSTREAM output ref (FLAG.on).
	{"disabled_map_ref", "disabled_map_ref", "expected/outs.json"},
	// Linear chains of two and three leaf stages, one task per stage.
	{"chain", "chain", "expected/outs.json"},
	{"chain3", "chain3", "expected/outs.json"},
	// SRC feeds one call and gates another via disabled = SRC.flag.
	{"chain_gate", "chain_gate", "expected/outs.json"},
	// The entry bakes skip=true, so the runtime gate skips GEN and its consumer
	// reads GEN's null output.
	{"disabled_entry_skip", "disabled_entry_skip", "expected/outs.json"},
	// #90: a trivial-compute fixture whose DAG mirrors CellRanger's
	// _basic_sc_rna_counter shape — preflight, split, a disable fan-out gating
	// aliased calls, a mapped per-sample stage, and nested sub-pipelines.
	{"cellranger_shaped", "cellranger_shaped", "expected/count_outs.json"},
	// #76: a return combining two calls, one of whose outputs is a file.
	{"native_file_return", "native_file_return", "expected/nf_outs.json"},
	// #328: marker-shaped string values (entry input, untyped map, call-site
	// literal, stage output, a file out naming no file) reach every stage and
	// the published outs as the literal strings mrp gives them.
	{"marker_strings", "marker_strings", "expected/outs.json"},
	// #284: stages see the basename mrp passes for a file from an upstream
	// stage, an entry input, a fork, a split's chunk def and a join's chunk outs.
	{"basename_echo", "basename_echo", "expected/outs.json"},
	// #337: a stage output nested 9,000 levels deep crosses tasks the driver
	// never parses.
	{"deep_json", "deep_json", "expected/outs.json"},
}

// TestGolden is the end-to-end differential suite (port of run.sh): transpile
// each fixture, run it under real Nextflow, and assert the published
// pipeline_outs.json matches the committed golden and the published file tree
// matches the one mrp published (assertGoldenTree). Cases run in
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

			goldenJSON(t, tc.fixture,
				filepath.Join(proj, "results", "pipeline_outs.json"),
				filepath.Join(fixtureDir, tc.golden))
			assertGoldenTree(t, tc.name, tc.fixture, proj)
		})
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

// TestCLITypedOverrides checks that scalar entry inputs set on the Nextflow
// command line reach the stage typed as declared. Nextflow passes an untyped
// --param as a String, and the stage reports each value's Python repr and
// type, so the CLI run matches the -params-file run and the mrp golden (the
// override values as invocation literals) only if mre plan typed `--b false`
// as the boolean false rather than the truthy string "false". A float or
// bool value that is not a literal of its declared type must fail the run at
// PLAN, before any stage runs, naming the input and its type: mre plan
// validates the launch values the params block leaves untyped, an int's
// included, as nextflow_schema.json validates nothing at launch. A params
// file's whole-number float for an int input runs as that int, as `mrp`
// takes the literal 5.0 for an int, where Nextflow's Integer refuses it.
func TestCLITypedOverrides(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	const fixture = "cli_typed"

	fixtureDir := filepath.Join(root, "testdata", fixture)
	golden := filepath.Join(fixtureDir, "expected", "override_outs.json")

	runs := []struct {
		name string
		args []string
		// whole is a params file written for the run, empty for none.
		whole string
	}{
		{name: "cli", args: []string{"--n", "5", "--f", "2.5", "--g", "1e6", "--b", "false"}},
		{name: "params_file", args: []string{"-params-file", "params.json"}},
		{name: "params_file_whole_int", args: []string{"-params-file", "whole.json"}, whole: `{"n": 5.0, "f": 2.5, "g": 1e6, "b": false}`},
	}

	for _, rc := range runs {
		t.Run(rc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, fixture)
			renderOverrideParams(t, fixtureDir, proj)

			if rc.whole != "" {
				writeFileT(t, filepath.Join(proj, "whole.json"), rc.whole)
			}

			if err := runNextflow(t, proj, rc.args...); err != nil {
				t.Fatal(err)
			}

			goldenJSON(t, fixture, filepath.Join(proj, "results", "pipeline_outs.json"), golden)
		})
	}

	for _, bad := range []struct{ param, typ, value string }{
		{"n", "int", "x"},
		{"f", "float", "2.5x"},
		{"b", "bool", "True"},
	} {
		t.Run("invalid_"+bad.typ, func(t *testing.T) {
			t.Parallel()

			assertPlanRefuses(t, transpile(t, fixture), []string{"--" + bad.param, bad.value},
				fmt.Sprintf("entry input %q declared %s, got %q", bad.param, bad.typ, bad.value))
		})
	}
}

// TestUndeclaredParamFailsAtLaunch (#357): a launch setting a param the
// project does not declare, on the command line or in a params file, such
// as a misspelt input, fails before any task, naming the param, where
// Nextflow would otherwise ignore it and run the .mro's value.
func TestUndeclaredParamFailsAtLaunch(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	const want = "Parameter `fatcor` was specified on the command line or params file but is not declared in the script or config"

	t.Run("cli", func(t *testing.T) {
		t.Parallel()

		assertLaunchRefuses(t, transpile(t, "cli_typed"), []string{"--fatcor", "3"}, want)
	})

	t.Run("params_file", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, "cli_typed")
		writeFileT(t, filepath.Join(proj, "params.json"), `{"n": 5, "fatcor": 3}`)
		assertLaunchRefuses(t, proj, []string{"-params-file", "params.json"}, want)
	})
}

// TestOutdirSetByALaterConfig: a -c config that sets params.outdir and not
// outputDir fails the run before PLAN, naming both, where the outputs would
// publish silently below the outputDir conf/base.config set; one that sets
// outputDir with it publishes there.
func TestOutdirSetByALaterConfig(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	t.Run("params_only", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, "cli_typed")
		writeFileT(t, filepath.Join(proj, "site.config"), "params.outdir = 'elsewhere'\n")
		assertLaunchRefuses(t, proj, []string{"-c", "site.config"},
			"params.outdir is elsewhere, set by a config after conf/base.config set outputDir from it, so the outputs would publish to /")
	})

	t.Run("with_output_dir", func(t *testing.T) {
		t.Parallel()

		proj := transpile(t, "cli_typed")
		writeFileT(t, filepath.Join(proj, "site.config"), "params.outdir = 'elsewhere'\noutputDir = params.outdir\n")

		if err := runNextflow(t, proj, "-c", "site.config"); err != nil {
			t.Fatal(err)
		}

		goldenJSON(t, "cli_typed", filepath.Join(proj, "elsewhere", "pipeline_outs.json"), filepath.Join(root, "testdata", "cli_typed", "expected", "outs.json"))
	})
}

// assertLaunchRefuses runs proj with args, which Nextflow must refuse
// before any task starts, with a run log holding want.
func assertLaunchRefuses(t *testing.T, proj string, args []string, want string) {
	t.Helper()

	if err := runNextflowAllowingFailedAttempts(t, proj, args...); err == nil {
		t.Fatalf("the run with %v must fail at launch", args)
	}

	runLog, err := os.ReadFile(filepath.Join(proj, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}

	if !strings.Contains(string(runLog), want) {
		t.Errorf("run log lacks %q:\n%s", want, runLog)
	}

	if strings.Contains(string(runLog), "PLAN") {
		t.Errorf("PLAN started:\n%s", runLog)
	}
}

// TestOutOfRangeLaunchValues (#567): a launch value outside Martian's number
// range fails the run at PLAN naming the input, as mrp's parser refuses the
// literal. A YAML params file keeps an int beyond int64 exact, and a JSON
// one a float beyond float64. The command line passes the int as a String,
// which the int input refuses.
func TestOutOfRangeLaunchValues(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	const fixture = "cli_typed"

	for _, tc := range []struct {
		name, file, body string
		args, wants      []string
	}{
		{
			name: "yaml_int", file: "params.yaml", body: "n: 9223372036854775808\n", args: []string{"-params-file", "params.yaml"},
			wants: []string{`entry input "n": number literal out of range: an int must fit int64, a float float64 and a threads, mem_gb or vmem_gb value float32: 9223372036854775808`},
		},
		{
			name: "yaml_float_int", file: "params.yaml", body: "f: 9223372036854775808\n", args: []string{"-params-file", "params.yaml"},
			wants: []string{`entry input "f": number literal out of range: an int must fit int64, a float float64 and a threads, mem_gb or vmem_gb value float32: 9223372036854775808`},
		},
		{
			name: "json_float", file: "params.json", body: `{"f": -1e400}`, args: []string{"-params-file", "params.json"},
			wants: []string{`entry input "f": number literal out of range: an int must fit int64, a float float64 and a threads, mem_gb or vmem_gb value float32: -1E+400`},
		},
		{
			name: "cli_int", args: []string{"--n", "9223372036854775808"},
			wants: []string{`entry input "n" declared int, got "9223372036854775808"`, "binding parameter n to value 9.223372036854776e+18"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, fixture)
			if tc.file != "" {
				writeFileT(t, filepath.Join(proj, tc.file), tc.body)
			}

			assertPlanRefuses(t, proj, tc.args, tc.wants...)
		})
	}
}

// assertPlanRefuses runs proj with args, which must fail the run at PLAN,
// before any other task, with a run log holding one of wants.
func assertPlanRefuses(t *testing.T, proj string, args []string, wants ...string) {
	t.Helper()

	if err := runNextflowAllowingFailedAttempts(t, proj, append(args, "-with-trace", "trace.txt")...); err == nil {
		t.Fatalf("the run with %v must fail at PLAN", args)
	}

	runLog, err := os.ReadFile(filepath.Join(proj, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}

	if !slices.ContainsFunc(wants, func(w string) bool { return strings.Contains(string(runLog), w) }) {
		t.Errorf("run log reports none of %q:\n%s", wants, runLog)
	}

	for _, r := range readTrace(t, filepath.Join(proj, "trace.txt")) {
		if r.Name != "PLAN" {
			t.Errorf("task %s ran after PLAN refused %v", r.Name, args)
		}
	}
}

// TestEntryFileAdversarialName checks a launch-time entry file override whose
// legal name holds `,`, `;` and `=` reaches the stage intact, through the
// params file, the driver and mre plan. The adversarial copy of entry_file's
// override input is created at runtime (committing such a basename would be
// fragile across tooling); the stage sums the file's numbers, so the run
// reproduces the override golden (total = 200) only if the path survived.
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

	goldenJSON(t, "entry_file",
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
