//go:build e2e

package e2e

// True differential test (port of mrp_diff.sh): run each fixture through REAL
// Martian (mrp) AND the transpiled Nextflow, then compare the published outs/
// tree — the set of output file paths relative to the outs dir plus their
// contents — the (path-normalized) _outs JSON, and every stage invocation's
// _args, _outs and _jobinfo allocation (abidiff_test.go). This validates mre's
// publish layout and the whole transpile+runtime path against Martian itself,
// not a hand-written golden.
//
// Requires a Martian build: set MARTIAN_BIN (default ~/workdir/martian/bin).
// Without one it skips locally and fails under E2E_REQUIRE=1, where CI builds
// it (.github/actions/setup-martian).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
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
// a bench/ width variant), with extra flags.
func runMrpAt(t *testing.T, bin, dir, mro, tmp string, extra ...string) {
	t.Helper()

	if out, err := execMrp(t.Context(), bin, dir, mro, tmp, extra...); err != nil {
		t.Fatalf("mrp %s: %v\n%s", filepath.Join(dir, mro), err, tail(out, 12))
	}
}

// errMrpUnfinished reports an mrp run its context stopped before it ended.
var errMrpUnfinished = errors.New("mrp did not finish")

// execMrp runs mrp as runMrpAt does and returns its combined output, which it
// also writes to <tmp>/mrp.log, and its error. When ctx ends first, mrp is
// sent SIGTERM, on which it stops its jobs, and the error wraps
// errMrpUnfinished.
func execMrp(ctx context.Context, bin, dir, mro, tmp string, extra ...string) ([]byte, error) {
	// disable keeps intermediate stage files, which the stage-ABI diff hashes
	// through the paths in each invocation's _args/_outs; mrp's default rolling
	// VDR deletes them once their consumers finish.
	return execMrpVDR(ctx, bin, dir, mro, tmp, "disable", extra...)
}

// The pool mrp's local job manager grants the differential's jobs from, its
// --localcores and --localmem, which a transpiled project's tasks are
// clamped to (mrpPoolArgs) so their jobs are granted the same.
const (
	mrpLocalCores = 2
	mrpLocalMemGB = 4
)

// mrpPoolArgs writes a config into proj that clamps its tasks to mrp's pool,
// and returns the flags that launch the project with it.
func mrpPoolArgs(t *testing.T, proj string) []string {
	t.Helper()

	path := filepath.Join(proj, "mrp-pool.config")
	body := fmt.Sprintf("process.resourceLimits = [cpus: %d, memory: %d.GB]\n", mrpLocalCores, mrpLocalMemGB)

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return []string{"-c", path}
}

// execMrpVDR is execMrp under the given --vdrmode.
func execMrpVDR(ctx context.Context, bin, dir, mro, tmp, vdrMode string, extra ...string) ([]byte, error) {
	// Preflight stages run (no --nopreflight) because mro2nf runs them, so their
	// invocations pair up.
	cmd := exec.CommandContext(ctx, filepath.Join(bin, "mrp"), slices.Concat([]string{
		filepath.Join(dir, mro), "mrp",
		"--jobmode=local", "--localcores=" + strconv.Itoa(mrpLocalCores), "--localmem=" + strconv.Itoa(mrpLocalMemGB),
		"--disable-ui", "--vdrmode=" + vdrMode,
	}, extra)...)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "MROPATH="+dir)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second

	out, err := cmd.CombinedOutput()

	_ = os.WriteFile(filepath.Join(tmp, "mrp.log"), out, 0o644)

	switch {
	case ctx.Err() != nil:
		return out, fmt.Errorf("run mrp: %w: %w", errMrpUnfinished, ctx.Err())
	case err != nil:
		return out, fmt.Errorf("run mrp: %w", err)
	default:
		return out, nil
	}
}

// nfResultsMeta is mro2nf's own metadata at the root of results/, the outs
// JSON. mrp keeps its outs JSON outside outs/, so a tree compared with mrp's
// drops it only under the allowlist's results-metadata rule, and only at the
// root, because a stage may publish a file of the same name deeper in the
// tree.
var nfResultsMeta = []string{"pipeline_outs.json"}

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
// invocation.
//
// mro names the entry invocation of a bench/ fixture, whose name is then its
// repo-relative dir (bench/<shape>); a testdata/ fixture leaves it empty and
// runs its pipeline.mro. cloud adds a leg run under cloudConfig, each task in
// its own scratch dir with its inputs copied in, as on an object-store work
// dir. overrides names an mrp --overrides file in the fixture's dir, which
// both runs are launched with.
type mrpDiffCase struct {
	name      string
	mro       string
	cloud     bool
	overrides string
}

// source returns the dir and entry .mro the case runs.
func (tc mrpDiffCase) source() (string, string) {
	if tc.mro != "" {
		return filepath.Join(root, tc.name), tc.mro
	}

	return filepath.Join(root, "testdata", tc.name), "pipeline.mro"
}

// flags are the case's launch flags of mrp and of Nextflow.
func (tc mrpDiffCase) flags() ([]string, []string) {
	dir, _ := tc.source()

	return overridesFlags(dir, tc.overrides)
}

// overridesFlags are the flags that launch mrp, and a transpiled project,
// with the overrides file named file in dir, or none.
func overridesFlags(dir, file string) ([]string, []string) {
	if file == "" {
		return nil, nil
	}

	path := filepath.Join(dir, file)

	return []string{"--overrides=" + path}, []string{"--mro2nf_overrides", path}
}

// mrpDiffCases are the fixtures run under both mrp and Nextflow; every other
// pipeline fixture is in mrpUnrunnable (coverage_test.go) with the reason.
var mrpDiffCases = []mrpDiffCase{
	{name: "file_min"},
	// #243: fractional mem_gb (and split-returned chunk/join mem) reaches the
	// stage as mrp resolves it, not Nextflow's truncated GB.
	{name: "frac_mem"},
	// #370: fractional threads reach the stage as mrp resolves them, not
	// Nextflow's integer cpus.
	{name: "frac_threads"},
	// #545: adaptive and oversized requests are granted from the pool, in
	// _jobinfo, _args and the thread variables, as mrp's local job manager
	// grants them.
	{name: "res_pool"},
	{name: "dir_out"},
	// #242: dotfiles and an empty subdirectory inside directory outputs.
	{name: "dir_hidden"},
	// #328: string values shaped like @mre: markers reach the stage and the
	// published outs exactly as mrp gives them.
	{name: "marker_strings"},
	// #284: every file a stage receives keeps the basename mrp passes it.
	{name: "basename_echo"},
	// #543: a stage opening a companion file beside its entry file input
	// finds it, as under mrp: every task reads a head-node path in place.
	{name: "entry_file_sibling"},
	{name: "map_file"},
	{name: "map_file_keyed"},
	{name: "map_split_file"},
	// #270: the determinism fixture's fork and chunk gathers match mrp's
	// order, not completion order.
	{name: "jitter_wide"},
	// #217: a split phase creates a custom-filetype file and returns it as a
	// per-chunk arg the JOIN reads.
	{name: "split_chunk_file"},
	// Coverage matrix: file leaves at the split/join chunk boundary — the
	// region #217 lived in. Nested struct chunk-def, array-of-files chunk-out,
	// and a directory chunk-out.
	{name: "split_struct_chunk"},
	// #245: split-returned chunk args reach main as written (5.0 stays a
	// float for an int chunk input).
	{name: "chunk_float_int"},
	{name: "chunk_out_filearr"},
	{name: "chunk_out_dir"},
	// Matrix completeness: the builtin `file` type (corpus only used custom
	// filetypes), and a file forwarded from a stage input into a split
	// chunk-arg (position 4).
	{name: "file_builtin_min"},
	{name: "split_forward_file"},
	{name: "struct_of_file"},
	{name: "struct_file_array"},
	// #173: a pipeline output typed as a callable's output struct (MAKEB),
	// bound whole, with a file leaf — the type walk needs the callable-derived
	// struct (addCallableStructs) to descend `made` to its `report` file.
	{name: "callable_struct_file"},
	// #240: whole refs to array- and map-forked calls bind one struct per
	// fork (Martian's resolveMerge), file leaves published per fork.
	{name: "map_wholeref"},
	// #172: array<map<Point>>.x projection — a map call over an array source
	// makes MAKEM.m an array<map<Point>>; projecting .x yields array<map<int>>.
	{name: "arr_map_proj"},
	{name: "diamond_min"},
	{name: "cli_typed"},
	{name: "fork_min"},
	{name: "struct_min"},
	// #239: struct narrowing on every bind path.
	{name: "struct_narrow"},
	// #366: every way a struct value is built reaches a stage and the outs
	// JSON in mrp's key order, through plain and forked calls.
	{name: "struct_key_order"},
	{name: "kitchen_sink"},
	{name: "file_tree"},
	{name: "map_null_map"},
	// #99: map-fork key ordering (mixed case/digit + astral + CJK-compat
	// keys) must agree with mrp's key order.
	{name: "map_key_sort"},
	// #99: an upstream map fork with real keys.
	{name: "map_fork_upstream"},
	// #595: B maps over A's outs, and A over a stage's: B's fork i reads
	// A's fork i.
	{name: "map_chain_runtime"},
	// #99 empty-fork fidelity, both sides of the static/runtime split:
	// an invocation-known empty split source merges statically, an upstream
	// empty/null collection at runtime, each to the typed empty.
	{name: "empty_fork_min"},
	{name: "empty_map_fork"},
	{name: "runtime_empty_forks"},
	// #127 value-chain empty-fork classes: mrp's static resolver merges
	// each (sub-pipeline chain to an entry value, in-pipeline cascade
	// through a mapped call, mixed entry+upstream zip).
	{name: "empty_fork_sub"},
	{name: "empty_fork_cascade"},
	{name: "empty_fork_mixed"},
	// #241: SUB at two call sites (invocation-known empty vs runtime empty).
	{name: "empty_fork_two_sites"},
	// #383: splits over disable-gated producers of invocation-known sources,
	// under literal and runtime gates; mrp merges each statically.
	{name: "empty_fork_gated"},
	{name: "empty_fork_gated_sub"},
	// #435: splits of a mapped call's whole ref over an invocation-known null
	// and empty, into a stage and into a sub-pipeline.
	{name: "empty_fork_wholeref"},
	// #436: splits of a disabled stage, sub-pipeline, map call and per-fork
	// stage, gated by entry values.
	{name: "disabled_split_null"},
	// #434: runtime-gated map calls over a sub-pipeline (array, map, empty and
	// null sources, in a keyed layer, into a split stage) against an
	// entry-gated one and a gated map call over a stage.
	{name: "disabled_map_nullforks"},
	// #433: a typed-map literal bound to a sub-pipeline input and split
	// through a field projection.
	{name: "map_literal_proj"},
	// #433: a map call's fork-independent calls run as often as mrp runs them,
	// across literal, entry, empty, runtime-empty, gated, nested, plain-sub
	// and keyed map calls.
	{name: "fork_indep_call"},
	// The committed expected/outs.json files of these claim mrp provenance,
	// so the live differential machine-checks that claim.
	{name: "fork_ref"},
	{name: "fork_mid"},
	{name: "fork_disabled_sub"},
	{name: "fork_disabled_skip"},
	// #221: `call SUB using (disabled=X)` over a sub-pipeline of one leaf
	// stage: _sub runs (skip=false → scaled=15), _skip nulls (skip=true →
	// scaled=null).
	{name: "disabled_plain_sub"},
	{name: "disabled_plain_skip"},
	// #339: a whole ref to a disabled stage and to a disabled sub-pipeline is
	// null in the consumer's _args.
	{name: "disabled_wholeref"},
	// #253: disable gates that are null at run time leave their calls
	// enabled on every gate path: upstream output and struct field, a
	// sub-pipeline input, and the gates of a map call over a sub-pipeline.
	{name: "null_gate"},
	// #559: a call gated by an unforked stage and a per-fork one, and
	// reading stages of its own fork and of none, each step keying its
	// instances for the next.
	{name: "gate_chain"},
	{name: "fork_fanout"},
	{name: "map_file_array"},
	// #99: a file-bearing leaf scatter.
	{name: "map_file_split"},
	// #90: CellRanger-shaped DAG (preflight, split, disable fan-out, aliasing,
	// map, nested pipelines) — all py stages, so it joins the mrp differential.
	{name: "cellranger_shaped"},
	// #113: stage src arguments (`src exec "code.py 3 hello"`) must reach
	// the stage under both runners. Unlike exec_min's stub, this one writes
	// journal entries (martian_shell.py update_journal), so real mrp
	// completes it.
	{name: "src_args"},
	// #238: float literals >= 1e6 (entry and call-site) reach the stage as
	// floats; the outputs are type+repr strings, since the _outs JSON diff
	// alone would equate 1e+06 with 1000000.
	{name: "literals_big"},
	// #612: float outputs keep mrp's lexemes (1e+21, -0.0, -0).
	{name: "float_lexemes"},
	// A comp stage split/main/join, which both runners run through mrjob.
	{name: "comp_split"},
	// The remaining runnable fixtures: each golden-suite shape faces live mrp,
	// not only its committed golden.
	{name: "alias_min"},
	{name: "api_smoke"},
	{name: "chain"},
	{name: "chain3"},
	{name: "chain_gate"},
	{name: "dead_map_pipe"},
	{name: "default_out"},
	{name: "disabled_callref"},
	{name: "disabled_entry_skip"},
	{name: "disabled_map"},
	{name: "disabled_map_ref"},
	{name: "fanin"},
	{name: "file_array"},
	{name: "file_chain"},
	{name: "float_to_int"},
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
	{name: "special_resource"},
	{name: "split_test"},
	{name: "stage_entry"},
	{name: "struct_input"},
	{name: "struct_proj"},
	{name: "typedmap_out"},
	{name: "vmem_stage"},
	{name: "wildcard"},
	// The stage holds 0.75 GB against its 0.5 GB request. Without --monitor
	// neither runner enforces that, so both succeed;
	// TestMonitorMemoryKillFailsLikeMrp covers the monitor kill.
	{name: "flaky_retry"},
	// #324: a split returning no chunks still runs the join.
	{name: "split_no_chunks"},
	// #332: the stage's output is a symlink to a host file, which mrp
	// publishes as a link and mro2nf as a copy (allowlist entry
	// published-symlinks-copied). #485: two outputs reach it, and each gets
	// its own link.
	{name: "symlink_escape"},
	// #431: a file reached from several outputs, through a pass-through stage
	// and a sub-pipeline boundary too, is named by the first path mrp
	// published it at.
	{name: "shared_out_file"},
	// #432: files below a 2-D array, reached through a struct projection and
	// a whole sub-pipeline ref, stay unpublished, as mrp leaves them. #457:
	// each is named in its producer's work dir, from a scratch dir too.
	{name: "nested_path_unpublished", cloud: true},
	// #341: a fork key holding a newline publishes its file, from a scratch
	// dir too, which Nextflow unstages a line at a time.
	{name: "map_key_newline", cloud: true},
	// #553: a fork's element of a txt[][] or map<txt[]> is a txt[], whose
	// files reach the forked stage from a scratch dir too.
	{name: "map_file_2d", cloud: true},
	// #314: a mapped and a disabled preflight run in DAG order under mro2nf.
	{name: "preflight_ungated"},
	// #314: a sub-pipeline's preflight, fed by a parent call, waits for its
	// parent's preflight and gates its sibling.
	{name: "preflight_order"},
	// #259: a forked call gathers its share of an inner map call, from a
	// stage's or the entry's source, also an empty one.
	{name: "map_nested_gather"},
	// #342: the modifiers mrp accepts on a top-level call.
	{name: "toplevel_stage_modifiers"},
	// #448: inside a map call over a sub-pipeline, a call reading only an
	// inner map call's fork runs once per inner fork.
	{name: "inner_only_fork"},
	// TestMrpDiffVDR also runs it under mrp's volatile data removal.
	{name: "vdr_volatile"},
	// A disable gate split per fork of a map call over a sub-pipeline: each
	// fork of the gated stage has its own gate value from the one producer.
	{name: "split_gate"},
	// The same gate under map calls the entry forks, an array and a map with
	// unsorted keys: the producer's outs give each fork its gate value.
	{name: "split_gate_entry"},
	// #337: a stage output nested 9,000 levels deep.
	{name: "deep_json"},
	// #472: every bench lane's invocation at its narrower width, so a
	// divergence on a bench shape fails here and not only the bench's
	// task-count gates.
	{name: "bench/chain", mro: "pipeline.mro"},
	{name: "bench/forks", mro: "pipeline_w4.mro"},
	{name: "bench/split", mro: "pipeline_c4.mro"},
	{name: "bench/mapsplit", mro: "pipeline_w4.mro"},
	{name: "bench/mapdisabled", mro: "pipeline_w4.mro"},
	{name: "bench/forkdisabled", mro: "pipeline_w4.mro"},
	{name: "bench/mapnested", mro: "pipeline_w2.mro"},
	{name: "bench/zip", mro: "pipeline_w4.mro"},
	{name: "bench/zipgated", mro: "pipeline_w4.mro"},
	{name: "bench/splitbroadcast", mro: "pipeline_w4.mro"},
	{name: "bench/mapchain", mro: "pipeline_w4.mro"},
	// #464: a map call zipping splits forks over its master source, and the
	// other sources read null past their end.
	{name: "zip_runtime"},
	{name: "zip_literal"},
	// #488: a map call zipping maps of different stages forks over its
	// first split's keys, and another map lacking a key reads null there.
	{name: "zip_runtime_map"},
	// A merge Martian expanded per fork of an outer map call gathers the
	// inner forks of its element's own outer fork (core ForkId.constrain),
	// an empty one as [], however the outer forks' widths differ: an inner
	// split over a stage output, or over a literal whose elements differ in
	// length (map_pipe_nested).
	{name: "merge_across_outer"},
	{name: "merge_across_empty"},
	{name: "merge_across_short"},
	{name: "map_pipe_nested"},
	// #387, #595: a split of a runtime-gated output, the forks fixed by the
	// gated call's own forks (rt_gate_split), by the gate alone over a
	// literal (_pass, _sub), and each with the gate on (rt_gate_split_on).
	{name: "rt_gate_split"},
	{name: "rt_gate_split_sub"},
	{name: "rt_gate_split_pass"},
	{name: "rt_gate_split_on"},
	// #595 (gen_54d3a5646ff7): the same inside a mapped sub-pipeline, the
	// split over an outer map call's gated element.
	{name: "rt_gate_split_nested"},
	// #601: a map call over a mapped sub-pipeline's merged outs, each fork
	// staging only the inner forks of its own outer fork.
	{name: "map_chain_partial"},
	// #601: reads only some forks of the literal read (mixed) and reads
	// through two roots (cross), each instance staging what it reads.
	{name: "map_chain_mixed"},
	{name: "map_chain_cross"},
	// #601: a merge's fork count read from a stage inside the merged
	// pipeline's own fork.
	{name: "map_chain_counted"},
	// #621: a literal Martian expands from an entry collection, read alike
	// at every width.
	{name: "witness_merged_literal"},
	// Two partner splits of one projected output inside a mapped
	// sub-pipeline, their references leaving the outer index unresolved
	// (gen-diff seed 862's witness).
	{name: "partner_nested_projection"},
	// #604 (gen-diff seed 2554's witness): a merge of a mapped sub-pipeline
	// Martian gave no fork root, gathering none of its forks, or its
	// compile-time forks.
	{name: "merge_unforked_pipeline"},
	// #613 (gen-diff seed 156120001705268): a merge constrained to a stage
	// with no root on the merged call gathers that stage's one root's
	// forks, which the pipeline outs wait for.
	{name: "merge_sub_root"},
	// #565: a top-level map call's outs are its pipeline's per fork, each
	// fork's files published under outs/<fork key>/.
	// TestMrpDiffSplitEntryOverride launches it with other split values.
	{name: "map_call_entry"},
	// #301: mrp's --overrides file at launch resizes each job as mrp does, by
	// keys scoped to a stage, a sub-pipeline, the top-level pipeline, an
	// aliased call and a map call, over a split's chunk and join requests.
	{name: "overrides_resources", overrides: "overrides.json"},
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
	// #314: a failing preflight runs alone; its siblings would fail too.
	{name: "preflight_fail", mrp: failedIn(failAssert, "CHECK")},
	{name: "bad_out_type", mrp: failedIn(failError, "BAD")},
	// A split returning a chunk resource key of the wrong JSON type fails the
	// split (#275): a number __special, a string __mem_gb or __threads.
	{name: "split_bad_special", mrp: failedIn(failError, "BAD_SPECIAL")},
	{name: "split_bad_mem", mrp: failedIn(failError, "BAD_MEM")},
	{name: "split_bad_threads", mrp: failedIn(failError, "BAD_THREADS")},
	// A relative entry path is passed through verbatim, so the stage opens
	// it from its own job dir, where it names nothing.
	{name: "entry_file", mrp: failedIn(failError, "COUNT")},
	{name: "entry_filearr", mrp: failedIn(failError, "COUNTARR")},
	{name: "entry_struct_file", mrp: failedIn(failError, "READCFG")},
	{name: "entry_mapfile", mrp: failedIn(failError, "COUNTMAP")},
	{name: "entry_dir", mrp: failedIn(failError, "COUNTDIR")},
	{name: "entry_file_empty", mrp: failedIn(failError, "SUMMARIZE")},
	{name: "split_from_file", mrp: failedIn(failError, "SQUARELINES")},
	{name: "mounted_paths", mrp: failedIn(failError, "MAKE_SHARD")},
	// #301: an overrides key mrp's reader does not know fails the run before
	// any stage runs.
	{name: "overrides_unknown", mrp: runOutcome{Failed: true}, overrides: "overrides.json"},
	// #496: outs reaching a directory and a file inside it. mrp succeeds and
	// leaves a dangling link; mro2nf fails the delivery.
	{name: "dir_and_inner_file", mrp: runOutcome{}, divergence: "dir-and-inner-file"},
	// #522: a nested map call whose inner forks are all empty. mrp panics
	// while it makes the pipestance, before any stage runs; mro2nf fails its
	// OUTS task.
	{name: "nested_empty_forks", mrp: runOutcome{Failed: true}, divergence: "nested-empty-forks"},
	// #602: a map call's forks vary with an outer map call its source's
	// producer is not forked on, over two elements. mrp fails resolving the
	// consumer's forks; mro2nf fails the producer's post.
	{name: "post_outer_fork", mrp: failedIn(failError, ""), divergence: "post-outer-fork"},
	// #520: a stage opening a companion file beside a file input another
	// stage wrote. mrp passes the path in the producer's job dir and finds
	// it; mro2nf stages only the files the producer's outs name, so the
	// stage fails.
	{name: "output_file_sibling", mrp: runOutcome{}, divergence: "input-file-siblings"},
	// An int beyond int64 in a JSON params file. mrp refuses the literal
	// before it makes a pipestance; Nextflow wraps the JSON int into int64,
	// and the run succeeds.
	{name: "params_json_int_wrap", mrp: runOutcome{Failed: true}, divergence: "params-json-int-wrap", params: "params.json", mro: "override_overflow.mro"},
	// #585: an entry input named with a Groovy keyword. mrp runs it; mro2nf
	// refuses the transpile (transpileRefused).
	{name: "keyword_input", mrp: runOutcome{}, divergence: "keyword-input"},
}

// TestMrpDiff runs each fixture under real mrp and under transpiled Nextflow
// and requires the published file tree, the _outs JSON and every stage
// invocation's _args, _outs and _jobinfo allocation to match.
func TestMrpDiff(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	for _, tc := range mrpDiffCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Gate before runMrp so one shard runs the fixture's mrp and its
			// Nextflow run (shardSkip's ancestor claim).
			shardSkip(t)
			runMrpDiffCase(t, bin, tc)
		})
	}
}

// TestMrpDiffSplitEntryOverride (#565): map_call_entry launched with its
// split input x in Martian's invocation form, {"split": v}, forks by v, an
// array of another length than the .mro's and a map, and matches mrp
// running the invocation whose literal is v.
func TestMrpDiffSplitEntryOverride(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	for _, tc := range []struct{ name, params, mro string }{
		{name: "array", params: `{"x": {"split": [4, 5]}}`, mro: "override_array.mro"},
		{name: "map", params: `{"x": {"split": {"r": 7, "q": 8}}}`, mro: "override_map.mro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runMrpDiffOverride(t, bin, "map_call_entry", tc.mro, "params.json", tc.params)
		})
	}
}

// TestMrpDiffInt64Override (#567): literals_edge launched with int64's min
// and max from a -params-file reaches the stage and the outs byte-exact, as
// mrp runs the invocation whose literals they are.
func TestMrpDiffInt64Override(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	runMrpDiffOverride(t, martianBin(t), "literals_edge", "override_int64.mro", "params.json",
		`{"neg": -9223372036854775808, "big": 9223372036854775807}`)
}

// TestMrpDiffFloatOverride: a float input's launch value reaches the stage
// and the outs byte-exact, as mrp runs the invocation whose literal it is:
// a whole number beyond 2^53 is Martian's int literal, which a float input
// takes as written, and a float takes Martian's shortest form (1e+06, -0).
// -0.0 comes from a YAML params file: Nextflow reads a JSON one's -0.0 as
// 0.0 before PLAN sees it.
func TestMrpDiffFloatOverride(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	for _, tc := range []struct{ name, file, params, mro string }{
		{name: "whole", file: "params.json", params: `{"f": 9007199254740993, "g": 1e6}`, mro: "override_float_whole.mro"},
		{name: "frac", file: "params.yaml", params: "f: 1.5\ng: -0.0\n", mro: "override_float_frac.mro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runMrpDiffOverride(t, bin, "cli_typed", tc.mro, tc.file, tc.params)
		})
	}
}

// runMrpDiffOverride runs fixture under Nextflow with the launch values
// params in the params file named file, and the invocation mro, whose
// literals are those values, under mrp, and diffs the two.
func runMrpDiffOverride(t *testing.T, bin, fixture, mro, file, params string) {
	t.Helper()
	shardSkip(t)

	dir := filepath.Join(root, "testdata", fixture)
	tmp := t.TempDir()
	runMrpAt(t, bin, dir, mro, tmp)

	proj := transpileMRO(t, dir, "pipeline.mro")
	writeFileT(t, filepath.Join(proj, file), params)

	if err := runNextflow(t, proj, append(mrpPoolArgs(t, proj), "-params-file", file)...); err != nil {
		t.Fatalf("nextflow: %v", err)
	}

	compareMrpToNextflow(t, fixture, tmp, proj)
	compareStageABI(t, fixture, tmp, proj)
}

// runMrpDiffCase runs one fixture under mrp and under Nextflow and diffs the
// two.
func runMrpDiffCase(t *testing.T, bin string, tc mrpDiffCase) {
	t.Helper()

	tmp := t.TempDir()
	dir, mro := tc.source()
	mrpFlags, nfFlags := tc.flags()
	runMrpAt(t, bin, dir, mro, tmp, mrpFlags...)

	compareStageABI(t, tc.name, tmp, diffNextflow(t, tc, tmp))

	// A scratch dir is gone after its task, and with it the stage metadata
	// the ABI comparison reads, so this leg compares the published outputs.
	if tc.cloud {
		t.Run("cloud", func(t *testing.T) {
			t.Parallel()

			proj := transpileMRO(t, dir, mro)
			writeCloudConfig(t, proj)

			if err := runNextflow(t, proj, append([]string{"-c", "cloud.config"}, nfFlags...)...); err != nil {
				t.Fatalf("nextflow: %v", err)
			}

			compareMrpToNextflow(t, tc.name, tmp, proj)
		})
	}
}

// diffNextflow transpiles a case's fixture, runs the generated project under
// Nextflow, diffs its published tree + _outs JSON against the mrp pipestance
// at <tmp>/mrp, and returns the project dir.
func diffNextflow(t *testing.T, tc mrpDiffCase, tmp string) string {
	t.Helper()

	dir, mro := tc.source()
	proj := transpileMRO(t, dir, mro)
	_, nfFlags := tc.flags()

	if err := runNextflow(t, proj, append(mrpPoolArgs(t, proj), nfFlags...)...); err != nil {
		t.Fatalf("nextflow: %v", err)
	}

	compareMrpToNextflow(t, tc.name, tmp, proj)

	return proj
}

// compareStageABI requires every stage phase's normalized _args, _outs and
// _jobinfo allocation to match between the mrp pipestance and the Nextflow
// work dir of fixture (#348), less what the allowlist excuses, and every
// allowlist entry scoped to the fixture to excuse something.
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
			mrpFlags, nfFlags := tc.flags(filepath.Join(root, "testdata", tc.name))
			mrp := mrpRunOutcome(t, bin, tc.name, tc.invocation(), tmp, mrpFlags...)
			nf, attempts, nfLog := nfRunOutcome(t, tc.name, nfFlags...)

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

// invocation is the .mro in the case's fixture dir mrp runs.
func (c mrpOutcomeCase) invocation() string {
	if c.mro == "" {
		return "pipeline.mro"
	}

	return c.mro
}

// flags are the launch flags of mrp's and the Nextflow run of the case's
// fixture in dir.
func (c mrpOutcomeCase) flags(dir string) ([]string, []string) {
	mrpFlags, nfFlags := overridesFlags(dir, c.overrides)
	if c.params != "" {
		nfFlags = append(nfFlags, "-params-file", filepath.Join(dir, c.params))
	}

	return mrpFlags, nfFlags
}

// mrpRunOutcome runs the invocation mro of a fixture under mrp in tmp, with
// extra flags, and classifies the run. Only mrp's non-zero exit counts as a
// failed run; mrp failing to start fails the test.
func mrpRunOutcome(t *testing.T, bin, fixture, mro, tmp string, extra ...string) runOutcome {
	t.Helper()

	o, err := mrpOutcome(t.Context(), bin, filepath.Join(root, "testdata", fixture), mro, tmp, extra...)
	if err != nil {
		t.Fatal(err)
	}

	return o
}

// nfRunOutcome is the mro2nf outcome of a fixture, the task name of every
// attempt (nextflowRunOutcome), and the log that explains it. A
// transpileRefused fixture must fail to transpile, so its outcome is a
// failure blaming no stage, with no attempts. Any other fixture is
// transpiled and run, with extra launch flags.
func nfRunOutcome(t *testing.T, fixture string, extra ...string) (runOutcome, []string, string) {
	t.Helper()

	reason, refused := transpileRefused[fixture]
	if !refused {
		proj := transpile(t, fixture)
		o, attempts := nextflowRunOutcome(t, proj, extra...)

		return o, attempts, tailFile(filepath.Join(proj, "run.log"))
	}

	buildBinaries(t)

	err := transpileInto(repoTools(), filepath.Join(root, "testdata", fixture), "pipeline.mro", t.TempDir())
	if err == nil {
		t.Fatalf("mro2nf transpiles %s; it must refuse it: %s", fixture, reason)
	}

	return failedWith(nil), nil, err.Error()
}

// mrpOutcome runs the invocation mro in dir under mrp in tmp, with extra
// flags, and classifies the run.
func mrpOutcome(ctx context.Context, bin, dir, mro, tmp string, extra ...string) (runOutcome, error) {
	_, err := execMrp(ctx, bin, dir, mro, tmp, extra...)
	if err == nil {
		return runOutcome{}, nil
	}

	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		return runOutcome{}, err
	}

	return mrpFailedOutcome(filepath.Join(tmp, "mrp"))
}

// nextflowRunOutcome runs a transpiled project and classifies the run from the
// FAILED attempts in its trace. It also returns the task name of every attempt
// the trace lists. args are the run's launch args.
func nextflowRunOutcome(t *testing.T, proj string, args ...string) (runOutcome, []string) {
	t.Helper()

	o, attempts, err := nextflowOutcome(proj, runNextflowAllowingFailedAttempts(t, proj, append([]string{"-with-trace", "trace.txt"}, args...)...))
	if err != nil {
		t.Fatal(err)
	}

	return o, attempts
}

// nextflowOutcome classifies the Nextflow run of proj that ended with runErr
// from the FAILED attempts in its trace.txt, and returns the task name of
// every attempt the trace lists. A failed run that wrote no trace ran no task,
// so it blames no stage.
func nextflowOutcome(proj string, runErr error) (runOutcome, []string, error) {
	rows, err := parseTrace(filepath.Join(proj, "trace.txt"))

	switch {
	case runErr != nil && errors.Is(err, fs.ErrNotExist):
		return failedWith(nil), nil, nil
	case err != nil:
		return runOutcome{}, nil, err
	}

	attempts := make([]string, len(rows))
	for i, r := range rows {
		attempts[i] = r.Name
	}

	if runErr == nil {
		return runOutcome{}, attempts, nil
	}

	var failed []failedAttempt

	for _, r := range rows {
		// PLAN starts the run as mrp starts one, compiling the invocation and
		// reading the overrides file, so its failure is mrp's refusal to make
		// a pipestance, which blames no stage (mrpFailedOutcome).
		if r.Status == "FAILED" && r.Name != "PLAN" {
			failed = append(failed, failedAttempt{Hash: r.Hash, Exit: r.Exit})
		}
	}

	o, err := nextflowFailedOutcome(filepath.Join(proj, "work"), failed)

	return o, attempts, err
}

// tailFile is tail for a log file, or a note that it could not be read.
func tailFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("    (%v)", err)
	}

	return tail(data, 12)
}
