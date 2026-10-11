//go:build e2e

package e2e

// True differential test (port of mrp_diff.sh): run each fixture through REAL
// Martian (mrp) AND the transpiled Nextflow, then compare the published outs/
// tree — the set of output file paths relative to the outs dir plus their
// contents — the (path-normalized) _outs JSON, and every stage invocation's
// _args/_outs (abidiff_test.go). This validates mre's
// publish layout and the whole transpile+runtime path against Martian itself,
// not a hand-written golden. Anchor fixtures additionally run -native (and
// -native -native-runner) Nextflow legs against the same mrp pipestance
// (#120), so the native emit path faces live mrp, not only committed goldens.
//
// Requires a Martian build: set MARTIAN_BIN (default ~/workdir/martian/bin).
// Without one it skips locally and fails under E2E_REQUIRE=1, where CI builds
// it (.github/actions/setup-martian).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// martianBin resolves the Martian bin dir and, like requireTools, skips the
// test when mrp is absent or fails it under E2E_REQUIRE=1.
func martianBin(tb testing.TB) string {
	tb.Helper()

	bin, err := lookupMartianBin()
	if err != nil {
		missingPrereqf(tb, "%v", err)
	}

	return bin
}

// errNoMrp reports that MARTIAN_BIN holds no executable mrp.
var errNoMrp = errors.New("mrp not found (set MARTIAN_BIN=<martian>/bin)")

// lookupMartianBin resolves the Martian bin dir from MARTIAN_BIN (default
// ~/workdir/martian/bin) and requires an executable mrp in it.
func lookupMartianBin() (string, error) {
	bin := os.Getenv("MARTIAN_BIN")
	if bin == "" {
		bin = filepath.Join(os.Getenv("HOME"), "workdir", "martian", "bin")
	}

	info, err := os.Stat(filepath.Join(bin, "mrp"))
	if err != nil || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s/mrp: %w", bin, errNoMrp)
	}

	return bin, nil
}

// TestMartianBinMissing checks a missing mrp fails the differential under
// E2E_REQUIRE=1 and skips it otherwise.
func TestMartianBinMissing(t *testing.T) {
	for _, require := range []string{"1", ""} {
		t.Run("E2E_REQUIRE="+require, func(t *testing.T) {
			t.Setenv("MARTIAN_BIN", t.TempDir())
			t.Setenv("E2E_REQUIRE", require)

			fake := &fakeTB{}
			martianBin(fake)

			if failed := fake.fatal != ""; failed != (require == "1") {
				t.Errorf("fatal=%q skip=%q, want a failure only under E2E_REQUIRE=1", fake.fatal, fake.skip)
			}

			if fake.fatal == "" && fake.skip == "" {
				t.Error("a missing mrp neither failed nor skipped the test")
			}
		})
	}
}

// runMrp runs the real Martian pipeline runner on a fixture inside tmp,
// producing <tmp>/mrp as the pipestance dir. mrp requires a plain pipestance
// name and creates it in the cwd, so it runs from tmp.
func runMrp(t *testing.T, bin, fixture, tmp string) {
	t.Helper()

	runMrpAt(t, bin, filepath.Join(root, "testdata", fixture), "pipeline.mro", tmp)
}

// runMrpAt is runMrp for the entry .mro of a fixture at an arbitrary dir (e.g.
// a bench/ width variant).
func runMrpAt(t *testing.T, bin, dir, mro, tmp string) {
	t.Helper()

	if out, err := execMrp(bin, dir, mro, tmp); err != nil {
		t.Fatalf("mrp %s: %v\n%s", filepath.Join(dir, mro), err, tail(out, 12))
	}
}

// execMrp runs mrp as runMrpAt does and returns its combined output, which it
// also writes to <tmp>/mrp.log, and its error.
func execMrp(bin, dir, mro, tmp string) ([]byte, error) {
	// disable keeps intermediate stage files, which the stage-ABI diff hashes
	// through the paths in each invocation's _args/_outs; mrp's default rolling
	// VDR deletes them once their consumers finish.
	return execMrpVDR(bin, dir, mro, tmp, "disable")
}

// execMrpVDR is execMrp under the given --vdrmode.
func execMrpVDR(bin, dir, mro, tmp, vdrMode string) ([]byte, error) {
	// Preflight stages run (no --nopreflight) because mro2nf runs them, so their
	// invocations pair up.
	cmd := exec.Command(filepath.Join(bin, "mrp"), filepath.Join(dir, mro), "mrp",
		"--jobmode=local", "--localcores=2", "--localmem=4", "--disable-ui", "--vdrmode="+vdrMode)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "MROPATH="+dir)

	out, err := cmd.CombinedOutput()

	_ = os.WriteFile(filepath.Join(tmp, "mrp.log"), out, 0o644)

	if err != nil {
		return out, fmt.Errorf("run mrp: %w", err)
	}

	return out, nil
}

// nfResultsMeta are mro2nf's own metadata files at the root of results/. mrp
// keeps its outs JSON outside outs/ and writes no manifest, so a tree compared
// with mrp's drops them only under the allowlist's results-metadata rule, and
// only at the root, because a stage may publish a file of the same name deeper
// in the tree.
var nfResultsMeta = []string{"pipeline_outs.json", "manifest.json.gz"}

// emptyDirMark is hashTree's value for an empty directory, keyed by its path
// plus a trailing slash so it cannot collide with a file's digest entry.
const emptyDirMark = "<empty dir>"

// linkMark prefixes publishedTree's value for a file reached through a
// symlink, so a published link and a published copy of its target differ.
const linkMark = "<symlink> "

// hashTree maps relpath -> sha256 for every file under dir, plus relpath/ ->
// emptyDirMark for every empty directory, skipping the rootMeta names at the
// top level. Symlinks are followed, so trees compare by content however they
// were materialised. An absent dir (a pipeline with no file outputs) yields an
// empty tree, matching mrp_diff.sh's tree() helper.
func hashTree(t *testing.T, dir string, rootMeta ...string) map[string]string {
	t.Helper()

	tree, err := publishedTree(dir, true, rootMeta)
	if err != nil {
		t.Fatal(err)
	}

	return tree
}

// publishedTree is hashTree's map, except that unless followLinks an entry
// reached through a symlink has linkMark before its value.
func publishedTree(dir string, followLinks bool, rootMeta []string) (map[string]string, error) {
	tree := map[string]string{}

	err := walkPublished(dir, rootMeta, func(rel, path string, emptyDir, linked bool) error {
		mark := ""
		if linked && !followLinks {
			mark = linkMark
		}

		if emptyDir {
			tree[rel+"/"] = mark + emptyDirMark

			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		sum := sha256.Sum256(data)
		tree[rel] = mark + hex.EncodeToString(sum[:])

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}

	return tree, nil
}

// errUnpublishable reports a tree entry that is neither a file nor a
// directory (a socket, device or named pipe).
var errUnpublishable = errors.New("not a regular file or directory")

// walkPublished calls visit for every file and every empty directory under
// dir (following symlinks), skipping the rootMeta names directly under dir.
// linked reports that the entry or a directory above it, below dir, is a
// symlink. An absent dir visits nothing.
func walkPublished(dir string, rootMeta []string, visit func(rel, path string, emptyDir, linked bool) error) error {
	if !fileExists(dir) {
		return nil
	}

	return walkPublishedRel(dir, "", false, rootMeta, visit)
}

func walkPublishedRel(path, rel string, linked bool, rootMeta []string, visit func(rel, path string, emptyDir, linked bool) error) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}

	if len(entries) == 0 && rel != "" {
		return visit(rel, path, true, linked)
	}

	for _, e := range entries {
		if rel == "" && slices.Contains(rootMeta, e.Name()) {
			continue
		}

		child, childRel := filepath.Join(path, e.Name()), filepath.Join(rel, e.Name())
		childLinked := linked || e.Type()&fs.ModeSymlink != 0

		info, err := os.Stat(child)
		if err != nil {
			return err
		}

		switch {
		case info.IsDir():
			err = walkPublishedRel(child, childRel, childLinked, nil, visit)
		case info.Mode().IsRegular():
			err = visit(childRel, child, false, childLinked)
		default:
			err = fmt.Errorf("%s (%s): %w", child, info.Mode(), errUnpublishable)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// mrpDiffCase is one fixture of the differential, run from its pipeline.mro
// invocation, plus the extra Nextflow legs it runs against the same mrp
// pipestance.
//
// native adds a -native transpile+Nextflow leg (#120): without it, -native
// correctness would rest entirely on committed goldens and a Martian behavior
// change at a pin bump — or a native-only divergence the golden JSON does not
// capture — would be invisible until goldens were regenerated. runner further
// adds a combined -native -native-runner leg; it is set wherever any py stage
// actually executes. The runner-less native fixtures run ZERO stage code, so a
// runner leg would re-test pure orchestration the native leg already covers:
// empty_fork_min/_sub/_cascade statically prune every fork (their only stage,
// SCALE, never runs) and fork_disabled_skip bakes skip=true so its whole
// sub-pipeline — SCALE again — is disabled. empty_fork_mixed is NOT such a
// case: its unconditional GEN call runs real stage code before the zero-fork
// zip, so it carries the runner leg. realMrjob runs the Nextflow side's comp
// stages through $MARTIAN_BIN/mrjob, the binary mrp itself runs them with.
type mrpDiffCase struct {
	name      string
	realMrjob bool
	native    bool
	runner    bool
	inline    bool
}

// mrpDiffCases are the fixtures run under both mrp and Nextflow; every other
// pipeline fixture is in mrpUnrunnable (coverage_test.go) with the reason.
var mrpDiffCases = []mrpDiffCase{
	{name: "file_min"},
	// #243: fractional mem_gb (and split-returned chunk/join mem) reaches the
	// stage as mrp resolves it, not Nextflow's truncated GB.
	{name: "frac_mem", native: true, runner: true},
	// #370: fractional threads reach the stage as mrp resolves them, not
	// Nextflow's integer cpus.
	{name: "frac_threads", native: true, runner: true},
	{name: "dir_out"},
	// #242: dotfiles and an empty subdirectory inside directory outputs,
	// through both LAYOUT variants (the transform return folds under -native).
	{name: "dir_hidden", native: true, runner: true},
	// #328: string values shaped like @mre: markers reach the stage and the
	// published outs exactly as mrp gives them.
	{name: "marker_strings", native: true, runner: true},
	// #284: every file a stage receives keeps the basename mrp passes it.
	{name: "basename_echo", native: true, runner: true},
	{name: "map_file"},
	{name: "map_file_keyed"},
	{name: "map_split_file"},
	// #270: the determinism fixture's fork and chunk gathers match mrp's
	// order, not completion order.
	{name: "jitter_wide"},
	// #217: a split phase creates a custom-filetype file and returns it as a
	// per-chunk arg the JOIN reads. Verifies output + native-runner parity;
	// the object-store staging path itself is guarded by the AWS e2e lane and
	// the cmd/mre readChunkData unit test (local/docker-iso share a fs).
	{name: "split_chunk_file", native: true, runner: true},
	// Coverage matrix: file leaves at the split/join chunk boundary — the
	// region #217 lived in. Nested struct chunk-def, array-of-files chunk-out,
	// and a directory chunk-out. Object-store staging verified on the AWS lane.
	{name: "split_struct_chunk", native: true, runner: true},
	// #245: split-returned chunk args reach main as written (5.0 stays a
	// float for an int chunk input), on the mre and native-runner paths.
	{name: "chunk_float_int", native: true, runner: true},
	{name: "chunk_out_filearr", native: true, runner: true},
	{name: "chunk_out_dir", native: true, runner: true},
	// Matrix completeness: the builtin `file` type (corpus only used custom
	// filetypes), and a file forwarded from a stage input into a split
	// chunk-arg (position 4). Same IsFile / chunk-def staging code paths the
	// AWS lane already proved on split_chunk_file / split_struct_chunk.
	{name: "file_builtin_min", native: true, runner: true},
	{name: "split_forward_file", native: true, runner: true},
	{name: "struct_of_file"},
	{name: "struct_file_array"},
	// #173: a pipeline output typed as a callable's output struct (MAKEB),
	// bound whole, with a file leaf — the type walk needs the callable-derived
	// struct (addCallableStructs) to descend `made` to its `report` file.
	{name: "callable_struct_file", native: true, runner: true},
	// #240: whole refs to array- and map-forked calls bind one struct per
	// fork (Martian's resolveMerge), file leaves published per fork.
	{name: "map_wholeref", native: true, runner: true},
	// #172: array<map<Point>>.x projection — a map call over an array source
	// makes MAKEM.m an array<map<Point>>; projecting .x yields array<map<int>>.
	{name: "arr_map_proj", native: true, runner: true},
	{name: "diamond_min"},
	{name: "cli_typed"},
	{name: "fork_min"},
	{name: "struct_min"},
	// #239: struct narrowing on every bind path; the noinline leg keeps the
	// NARROW sub's return a would-be forward, the native legs fold binds.
	{name: "struct_narrow", native: true, runner: true, inline: true},
	{name: "kitchen_sink", inline: true},
	{name: "file_tree", inline: true},
	{name: "map_null_map"},
	// #99: map-fork key ordering (mixed case/digit + astral + CJK-compat
	// keys) — the driver's UTF-8 byte sort must agree with mrp's key order.
	{name: "map_key_sort", native: true, runner: true},
	// #99: upstream map-fork element path (producer bundle sliced, real keys).
	{name: "map_fork_upstream"},
	// #99 empty-fork fidelity, both sides of the static/runtime split:
	// an invocation-known empty split source merges to null (mrp's static
	// resolver prunes the zero-fork call); an upstream empty/null
	// collection merges to the typed empty at runtime.
	{name: "empty_fork_min", native: true},
	{name: "empty_map_fork"},
	{name: "runtime_empty_forks", native: true, runner: true},
	// #127 value-chain empty-fork classes: mrp's static resolver prunes
	// each to null (sub-pipeline chain to an entry value, in-pipeline
	// cascade through a mapped call, mixed entry+upstream zip) — the
	// live differential machine-checks the knownInvocation cascade.
	{name: "empty_fork_sub", native: true, inline: true},
	{name: "empty_fork_cascade", native: true},
	{name: "empty_fork_mixed", native: true, runner: true},
	// #241: SUB at two call sites (invocation-known empty vs runtime empty),
	// disable-gated so every leg keeps both SUB boundaries.
	{name: "empty_fork_two_sites", native: true, inline: true},
	// #383: splits over disable-gated producers of invocation-known sources,
	// under literal and runtime gates; mrp prunes each to null.
	{name: "empty_fork_gated", native: true, runner: true, inline: true},
	{name: "empty_fork_gated_sub", native: true, runner: true, inline: true},
	// The native-suite golden anchors: their committed expected/outs.json
	// files claim mrp provenance, so the live differential machine-checks
	// that claim — otherwise TestGolden + the native suites would prove
	// only self-consistency. Their native legs face the -native transpile
	// itself against live mrp, not just the golden snapshot.
	{name: "fork_ref", native: true, runner: true},
	{name: "fork_mid", native: true, runner: true},
	// The inline legs here keep the disabled SUB a boundary (its internal call
	// is a MAP call, which the profitability guard excludes), so they gate that
	// -inline-pipelines leaves a non-flat disabled sub byte-identical.
	{name: "fork_disabled_sub", native: true, runner: true, inline: true},
	{name: "fork_disabled_skip", native: true, inline: true},
	// #221 disabled-plain sub-pipeline inlining: `call SUB using (disabled=X)`
	// where SUB is a FLAT plain sub-pipeline (one undisabled leaf stage reading
	// only pipeargs) splices its internal stage into the parent gated by X — the
	// profitability guard's win case. _sub runs (skip=false → scaled=15), _skip
	// nulls (skip=true → scaled=null), so the inline leg proves both the run and
	// the disabled-null branch stay byte-identical to mrp.
	{name: "disabled_plain_sub", inline: true},
	{name: "disabled_plain_skip", inline: true},
	// #339: a whole ref to a disabled stage and to a disabled (flat,
	// otherwise inlinable) sub-pipeline is null in the consumer's _args; the
	// noinline leg covers the boundary path, the default leg the inliner.
	{name: "disabled_wholeref", native: true, inline: true},
	{name: "fork_fanout", native: true, runner: true},
	{name: "map_file_array", native: true, runner: true},
	// #99: file-bearing leaf scatter — the FORK-resolve + folded-merge path.
	{name: "map_file_split"},
	// #90: CellRanger-shaped DAG (preflight, split, disable fan-out, aliasing,
	// map, nested pipelines) — all py stages, so it joins the mrp differential.
	{name: "cellranger_shaped", native: true, runner: true, inline: true},
	// #113: stage src arguments (`src exec "code.py 3 hello"`) must reach
	// the stage under both runners. Unlike exec_min's stub, this one writes
	// journal entries (martian_shell.py update_journal), so real mrp
	// completes it.
	{name: "src_args"},
	// #238: float literals >= 1e6 (entry and call-site) reach the stage as
	// floats; the outputs are type+repr strings, since the _outs JSON diff
	// alone would equate 1e+06 with 1000000. The runner leg covers the
	// native runner's own json.load of the args bundle.
	{name: "literals_big", native: true, runner: true},
	// A comp stage split/main/join: mrp runs it through its own mrjob, so the
	// Nextflow side does too rather than through the fixture's mrjob.sh stub.
	{name: "comp_split", realMrjob: true},
	// The remaining runnable fixtures: each golden-suite shape faces live mrp,
	// not only its committed golden.
	{name: "alias_min"},
	{name: "api_smoke"},
	{name: "chain_fuse"},
	{name: "chain_fuse3"},
	{name: "chain_fuse_disable"},
	{name: "dead_map_pipe"},
	{name: "default_out"},
	{name: "disabled_callref"},
	{name: "disabled_map"},
	{name: "disabled_map_ref"},
	{name: "fanin"},
	{name: "file_array"},
	{name: "file_chain"},
	{name: "float_to_int"},
	{name: "fold_disable"},
	{name: "fork_upstream"},
	{name: "gpu_stage"},
	{name: "include_test"},
	{name: "join_resources"},
	{name: "literals"},
	{name: "literals_edge"},
	{name: "map_fork"},
	{name: "map_pipe"},
	{name: "map_pipe_disabled"},
	{name: "map_pipe_disabled_nested"},
	{name: "map_pipe_split"},
	{name: "map_split"},
	{name: "map_struct_proj"},
	{name: "modifiers_min"},
	{name: "multidim"},
	{name: "multisplit"},
	{name: "native_file_return"},
	{name: "nested_struct"},
	{name: "null_in"},
	{name: "returnonly"},
	{name: "special_resource", native: true, runner: true},
	{name: "split_test"},
	{name: "stage_entry"},
	{name: "struct_input"},
	{name: "struct_proj"},
	{name: "typedmap_out"},
	{name: "vmem_stage"},
	{name: "wildcard"},
	// The stage holds 0.75 GB against its 0.5 GB request. Without -monitor
	// neither runner enforces that, so both succeed;
	// TestOrdinaryFailureRetriesWithEscalatedMemory covers the -monitor kill.
	{name: "flaky_retry"},
	// #324: a split returning no chunks still runs the join.
	{name: "split_no_chunks"},
	// #332: the stage's output is a symlink to a host file, which mrp
	// publishes as a link and mro2nf as a copy (allowlist entry
	// published-symlinks-copied).
	{name: "symlink_escape"},
	// #314: a mapped and a disabled preflight run in DAG order under mro2nf.
	{name: "preflight_ungated"},
	// TestMrpDiffVDR also runs it under mrp's volatile data removal.
	{name: "vdr_volatile"},
}

// mrpOutcomeCases are the fixtures whose run outcome (outcome_test.go), not
// their outputs, is compared with mrp's by TestMrpDiffFailureParity: mrp
// fails each by design, or mro2nf's outcome is a known divergence from mrp's,
// named by its allowlist entry in divergences.json. Fixing the divergence fails
// the case until the entry is deleted.
var mrpOutcomeCases = []mrpOutcomeCase{
	// An ASSERT in each phase: main, split and join.
	{name: "assert_min", mrp: failedIn(failAssert, "BOOM")},
	{name: "assert_split", mrp: failedIn(failAssert, "SPLIT_BOOM")},
	{name: "assert_join", mrp: failedIn(failAssert, "JOIN_BOOM")},
	// An ordinary exception is an error, never an ASSERT, however often
	// mro2nf retries it.
	{name: "stage_raise", mrp: failedIn(failError, "RAISE")},
	// mrp's output validation fails the producer before its consumer binds
	// the bad value: a missing struct key (#274), a float in an `out int`
	// (#338).
	{name: "missing_struct_key", mrp: failedIn(failError, "UP")},
	{name: "bad_out_type", mrp: failedIn(failError, "BAD")},
	// A split returning a chunk resource key of the wrong JSON type fails the
	// split (#275): a number __special, a string __mem_gb or __threads.
	{name: "split_bad_special", mrp: failedIn(failError, "BAD_SPECIAL")},
	{name: "split_bad_mem", mrp: failedIn(failError, "BAD_MEM")},
	{name: "split_bad_threads", mrp: failedIn(failError, "BAD_THREADS")},
	// mrp refuses to instantiate a split of a runtime-gated literal collection
	// (#387), and mro2nf refuses to transpile it (transpileRefused).
	{name: "rt_gate_split_reject", mrp: runOutcome{Failed: true}},
	{name: "rt_gate_split_reject_sub", mrp: runOutcome{Failed: true}},
	{name: "rt_gate_split_reject_pass", mrp: runOutcome{Failed: true}},
	// The reverse: mrp runs WORK behind a runtime-null disable gate, mro2nf's
	// driver aborts on it. Once fixed, the fixture moves to mrpDiffCases.
	{
		name:       "null_disable_gate",
		mrp:        runOutcome{},
		divergence: "runtime-null-disable-gate-aborts",
	},
}

// TestMrpDiff runs each fixture under real mrp and under transpiled Nextflow
// and requires the published file tree, the _outs JSON and every stage
// invocation's _args/_outs to match.
func TestMrpDiff(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	for _, tc := range mrpDiffCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Gate before runMrp so one shard runs the fixture's mrp and all its
			// legs (shardSkip's ancestor claim).
			shardSkip(t)
			runMrpDiffCase(t, bin, tc)
		})
	}
}

// runMrpDiffCase runs one fixture under mrp once and diffs every requested
// Nextflow leg against that pipestance.
func runMrpDiffCase(t *testing.T, bin string, tc mrpDiffCase) {
	t.Helper()

	tmp := t.TempDir()
	runMrp(t, bin, tc.name, tmp)

	// Extra transpile flags land AFTER the fixture's auto-added -mrjob;
	// mro2nf's stdlib flag parsing keeps the LAST occurrence, so the real mrjob
	// overrides the fixture stub.
	var extra []string

	if tc.realMrjob {
		mrjob := filepath.Join(bin, "mrjob")
		if !fileExists(mrjob) {
			missingPrereqf(t, "mrjob not found at %s", mrjob)
		}

		extra = append(extra, "-mrjob", mrjob)
	}

	compareStageABI(t, tc.name, tmp, diffNextflow(t, tc.name, tmp, extra))

	if tc.native {
		t.Run("native", func(t *testing.T) {
			t.Parallel()
			compareStageABI(t, tc.name, tmp, diffNextflow(t, tc.name, tmp, append(slices.Clone(extra), "-native")))
		})
	}

	// No stage-ABI diff here: the runner calls the stage functions in-process
	// and writes no _args/_outs, so there is no adapter ABI to compare.
	if tc.runner {
		t.Run("native_runner", func(t *testing.T) {
			t.Parallel()
			diffNextflow(t, tc.name, tmp, append(slices.Clone(extra), "-native", "-native-runner"))
		})
	}

	// #221: inlining sub-pipeline boundaries is ON by default (the default leg
	// already exercises it). This leg gates the OPT-OUT: the per-boundary-task
	// path (-inline-pipelines=false) must ALSO stay byte-identical to the same
	// mrp pipestance.
	if tc.inline {
		t.Run("noinline", func(t *testing.T) {
			t.Parallel()
			compareStageABI(t, tc.name, tmp, diffNextflow(t, tc.name, tmp, append(slices.Clone(extra), "-inline-pipelines=false")))
		})
	}
}

// diffNextflow transpiles a fixture with the given flags, runs the generated
// project under Nextflow, diffs its published tree + _outs JSON against the mrp
// pipestance at <tmp>/mrp, and returns the project dir.
func diffNextflow(t *testing.T, fixture, tmp string, flags []string) string {
	t.Helper()

	proj := transpile(t, fixture, flags...)

	if err := runNextflow(t, proj); err != nil {
		t.Fatalf("nextflow: %v", err)
	}

	compareMrpToNextflow(t, fixture, tmp, proj)

	return proj
}

// compareStageABI requires every stage phase's normalized _args and _outs to
// match between the mrp pipestance and the Nextflow work dir of fixture
// (#348), less what the allowlist excuses, and every allowlist entry scoped to
// the fixture to excuse something.
func compareStageABI(t *testing.T, fixture, tmp, proj string) {
	t.Helper()

	reports, err := diffStageABI(mustAllowlist(t), fixture, filepath.Join(tmp, "mrp"), proj)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range reports {
		t.Errorf("stage ABI differs from mrp: %s", r)
	}
}

// compareMrpToNextflow diffs the published file tree (paths + contents) and
// the outs JSON between fixture's mrp pipestance at <tmp>/mrp and a Nextflow
// project's results/, less what the allowlist excuses, and requires every
// issue-tracked rule entry scoped to fixture to excuse something
// (published_test.go).
func compareMrpToNextflow(t *testing.T, fixture, tmp, proj string) {
	t.Helper()

	reports, err := diffPublished(mustAllowlist(t), fixture, filepath.Join(tmp, "mrp"), filepath.Join(proj, "results"))
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range reports {
		t.Error(r)
	}
}

// TestMrpDiffFailureParity runs each mrpOutcomeCases fixture under real mrp and
// under transpiled Nextflow and requires both runs to end the same way: both
// succeed, or both fail and blame the same stages with the same failure kinds
// (outcome_test.go says what is compared and why).
func TestMrpDiffFailureParity(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)
	allow := mustAllowlist(t)

	for _, tc := range mrpOutcomeCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			d, err := tc.validate(allow)
			if err != nil {
				t.Fatal(err)
			}

			tmp := t.TempDir()
			mrp := mrpRunOutcome(t, bin, tc.name, tmp)
			nf, attempts, nfLog := nfRunOutcome(t, tc.name)

			reports := append(tc.check(d, mrp, nf), outcomeRuleReports(allow, tc.name, attempts)...)
			for _, r := range reports {
				t.Error(r)
			}

			if len(reports) > 0 {
				t.Logf("mrp.log:\n%s\nmro2nf:\n%s", tailFile(filepath.Join(tmp, "mrp.log")), nfLog)
			}
		})
	}
}

// mrpRunOutcome runs a fixture under mrp in tmp and classifies the run. Only
// mrp's non-zero exit counts as a failed run; mrp failing to start fails the
// test.
func mrpRunOutcome(t *testing.T, bin, fixture, tmp string) runOutcome {
	t.Helper()

	_, err := execMrp(bin, filepath.Join(root, "testdata", fixture), "pipeline.mro", tmp)
	if err == nil {
		return runOutcome{}
	}

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}

	o, err := mrpFailedOutcome(filepath.Join(tmp, "mrp"))
	if err != nil {
		t.Fatal(err)
	}

	return o
}

// nfRunOutcome is the mro2nf outcome of a fixture, the task name of every
// attempt (nextflowRunOutcome), and the log that explains it. A
// transpileRefused fixture must fail to transpile in every mode, the
// counterpart of mrp refusing it before it makes a pipestance, so its outcome
// is a failure blaming no stage, with no attempts. Any other fixture is
// transpiled and run.
func nfRunOutcome(t *testing.T, fixture string) (runOutcome, []string, string) {
	t.Helper()

	if _, refused := transpileRefused[fixture]; !refused {
		proj := transpile(t, fixture)
		o, attempts := nextflowRunOutcome(t, proj)

		return o, attempts, tailFile(filepath.Join(proj, "run.log"))
	}

	buildBinaries(t)

	var logs []string

	for _, flags := range [][]string{nil, {"-native"}, {"-inline-pipelines=false"}} {
		err := transpileInto(filepath.Join(root, "testdata", fixture), "pipeline.mro", t.TempDir(), flags...)
		if err == nil {
			t.Fatalf("mro2nf transpiles %s under %v; mrp refuses it before any stage runs", fixture, flags)
		}

		logs = append(logs, fmt.Sprintf("%v: %v", flags, err))
	}

	return failedWith(nil), nil, strings.Join(logs, "\n")
}

// nextflowRunOutcome runs a transpiled project and classifies the run from the
// FAILED attempts in its trace. It also returns the task name of every attempt
// the trace lists.
func nextflowRunOutcome(t *testing.T, proj string) (runOutcome, []string) {
	t.Helper()

	runErr := runNextflowAllowingFailedAttempts(t, proj, "-with-trace", "trace.txt")
	rows := readTrace(t, filepath.Join(proj, "trace.txt"))

	attempts := make([]string, len(rows))
	for i, r := range rows {
		attempts[i] = r.Name
	}

	if runErr == nil {
		return runOutcome{}, attempts
	}

	var failed []failedAttempt

	for _, r := range rows {
		if r.Status == "FAILED" {
			failed = append(failed, failedAttempt{Hash: r.Hash, Exit: r.Exit})
		}
	}

	o, err := nextflowFailedOutcome(filepath.Join(proj, "work"), failed)
	if err != nil {
		t.Fatal(err)
	}

	return o, attempts
}

// tailFile is tail for a log file, or a note that it could not be read.
func tailFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("    (%v)", err)
	}

	return tail(data, 12)
}
