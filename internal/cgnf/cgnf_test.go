package cgnf

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func emitFixture(t *testing.T, name string) string {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	opts := Options{OutDir: out, Mre: "/opt/mre", Mrjob: "/opt/mrjob", Adapters: "/opt/adapters", MroPaths: []string{dir}, Version: "test"}

	if err := Emit(filepath.Join(dir, "pipeline.mro"), opts); err != nil {
		t.Fatal(err)
	}

	return out
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		out[rel] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// TestEmitProject: a project is main.nf wiring, the one stage module, the
// config and the transpiled source; it does not depend on where it is
// emitted.
func TestEmitProject(t *testing.T) {
	a, b := readTree(t, emitFixture(t, "split_test")), readTree(t, emitFixture(t, "split_test"))

	if d := cmp.Diff(a, b); d != "" {
		t.Errorf("two emissions differ:\n%s", d)
	}

	want := []string{"_assets/source.json", "_assets/types.json", "main.nf", "nextflow.config", "stage.nf"}

	if d := cmp.Diff(want, slices.Sorted(maps.Keys(a))); d != "" {
		t.Errorf("project files (-want +got):\n%s", d)
	}

	if a["stage.nf"] != stageModule {
		t.Error("stage.nf is not the stage module")
	}
}

// TestEmitAliases: every stage node of the witness's call graph is one
// alias of the stage module, a split stage the split, main and join
// processes, and every entry-fed fork root is one PLAN output
// (map_fork_upstream: GEN, then DBL forked on GEN's map).
func TestEmitAliases(t *testing.T) {
	main := readTree(t, emitFixture(t, "split_test"))["main.nf"]

	if !strings.Contains(main, "include { STAGE_SPLIT as N0_SUM_SQUARES_SP ; STAGE_MAIN as N0_SUM_SQUARES_MN ; STAGE_JOIN as N0_SUM_SQUARES_JN } from './stage.nf'") {
		t.Errorf("split_test's split stage is not the split triad:\n%s", main)
	}

	up := readTree(t, emitFixture(t, "map_fork_upstream"))["main.nf"]

	for _, want := range []string{
		"include { STAGE as N0_GEN } from './stage.nf'",
		"include { STAGE as N1_DBL } from './stage.nf'",
		"o0_forks.flatMap { _kp, ds -> ds.findAll { e -> e.parent.name == 'MFU.DBL' }",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("map_fork_upstream's main.nf lacks %q:\n%s", want, up)
		}
	}

	if strings.Contains(up, "emit: r0") {
		t.Errorf("map_fork_upstream has no entry-fed root, yet PLAN emits one:\n%s", up)
	}

	nested := readTree(t, emitFixture(t, "map_pipe_nested"))["main.nf"]
	if !strings.Contains(nested, "emit: r0") || !strings.Contains(nested, "emit: r1") {
		t.Errorf("map_pipe_nested's two entry-fed roots are not two PLAN outputs:\n%s", nested)
	}
}

// TestEmitGPUSelectors: `special = "gpu[:N]"` requests N GPUs on the alias
// that runs the stage's code, a split stage's chunks (gpu_stage: INFER one,
// TRAIN two per chunk).
func TestEmitGPUSelectors(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "gpu_stage"))["nextflow.config"]

	for _, want := range []string{
		"withName: 'N0_INFER' {\n        accelerator = 1\n    }",
		"withName: 'N1_TRAIN_MN' {\n        accelerator = 2\n    }",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}
}

// TestEmitOrdersGathers: every list a task stages from several tasks is in
// an order fixed by the run, not by completion, as that order is part of
// the task's -resume key: a join's chunk outs by chunk name (split_test), a
// gather by the upstream instance's key (fork_mid: SUMALL over SCALE's
// forks), and OUTS's inputs by node and key.
func TestEmitOrdersGathers(t *testing.T) {
	split := readTree(t, emitFixture(t, "split_test"))["main.nf"]
	gather := readTree(t, emitFixture(t, "fork_mid"))["main.nf"]

	for _, c := range []struct{ fixture, main, want string }{
		{"split_test", split, "groupTuple().map { gk, os -> [gk.getGroupTarget(), os.sort(false) { a, b -> a.name <=> b.name }] }"},
		{"fork_mid", gather, ".collect { e -> [ku, e] }] }).groupTuple().map { c, dss -> [c, ordered(dss.findAll { x -> x != null }.collectMany { x -> x })] }"},
		{"split_test", split, "o0_outs.flatMap { k, ds -> ds.findAll { e -> e.name in ['sum'] }.collect { e -> [[0] + k, e] } }).toList().map { xs -> ordered(xs) }"},
	} {
		if !strings.Contains(c.main, c.want) {
			t.Errorf("%s's main.nf lacks %q:\n%s", c.fixture, c.want, c.main)
		}
	}
}

// TestEmitStagesChunkApart: a chunk's main stages the split's chunk dir
// below chunk/, apart from the job's metadata dir of the chunk's name, so
// the job writes nothing into the split's outputs, which would change their
// -resume key (split_test).
func TestEmitStagesChunkApart(t *testing.T) {
	if !strings.Contains(stageModule, "tuple val(meta), val(key), path('args'), val(name), path(chunk, stageAs: 'chunk/*')") {
		t.Error("STAGE_MAIN does not stage its chunk below chunk/")
	}
}

// TestEmitSizesChunksAndJoin: each chunk's main and the join are scheduled
// with the resources the split returned for them, mrp's per-chunk and join
// reservations (join_resources: the join requests 2 threads and 3 GB).
func TestEmitSizesChunksAndJoin(t *testing.T) {
	main := readTree(t, emitFixture(t, "join_resources"))["main.nf"]

	for _, want := range []string{
		"cs.collect { c -> [sized(m, jsonOf(c.resolve('chunk.json')).resources) + [chunks: cs.size()], k, a, c.name, c] }",
		"s0.map { m, k, a, cs, j -> [k, sized(m, jsonOf(j)), a, cs, j] }",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("join_resources's main.nf lacks %q:\n%s", want, main)
		}
	}
}

// TestEmitRetriesWhatMrpRetries: a task is retried, as many times as mrp
// retries a job, only on mre's transient exit or a signal's, never on a
// stage exception, an ASSERT or a failed mre step; and under -monitor a
// retry keeps the stage's own memory, at which mrjob kills it as mrp does.
func TestEmitRetriesWhatMrpRetries(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "split_test"))["nextflow.config"]

	for _, want := range []string{
		"errorStrategy = { task.exitStatus in [75, 137, 143] ? 'retry' : 'terminate' }",
		"maxRetries = 2",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	if !strings.Contains(stageModule, `"${params.mro2nf_monitor ? meta.mem_gb : meta.mem_gb * attempt} GB"`) {
		t.Error("stage.nf escalates a retry's memory under -monitor")
	}
}

// TestEmitRefusesReservedParam: an entry input named like a param the
// project defines itself is refused.
func TestEmitRefusesReservedParam(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	src := "stage S(\n    in  int outdir,\n    out int y,\n    src py \"s\",\n)\n\ncall S(\n    outdir = 1,\n)\n"

	if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Emit(mro, Options{OutDir: t.TempDir(), MroPaths: []string{dir}})
	if !errors.Is(err, errReservedParam) {
		t.Errorf("Emit = %v, want errReservedParam", err)
	}
}

// TestEmitZipsByToken: an instance zipping another stage's output stages
// that stage's partner element at its own fork's index token, or the
// absent record, never the whole output (zip_runtime: LONG zips THREE.xs;
// zip_runtime_map: BY_AX zips ABC.m by key token).
func TestEmitZipsByToken(t *testing.T) {
	for _, c := range []struct{ fixture, root, partner, token string }{
		{"zip_runtime", "ZIP_RUNTIME.LONG", "ZIP_RUNTIME.LONG@ZIP_RUNTIME.THREE.xs", "t[-1].toString()"},
		{"zip_runtime_map", "ZIP_RUNTIME_MAP.BY_AX", "ZIP_RUNTIME_MAP.BY_AX@ZIP_RUNTIME_MAP.ABC.m", "e.name.tokenize('@')[1]"},
	} {
		main := readTree(t, emitFixture(t, c.fixture))["main.nf"]

		for _, want := range []string{
			"e.parent.name == '" + c.root + "' }.collect { e -> def t = e.name.tokenize('@')[0].tokenize('.').collect { x -> x as int }; [[], t[-1], e, " + c.token + "] }",
			"ds.findAll { e -> e.parent.name == '" + c.partner + "' }.collectEntries { e -> [(e.name): e] }] }, by: 0).map { _q, k, fs, es, tk, m -> [k, fs, es + ([m[tk[0]] ?: m['none']] - [null]), tk] }",
		} {
			if !strings.Contains(main, want) {
				t.Errorf("%s's main.nf lacks %q:\n%s", c.fixture, want, main)
			}
		}

		if strings.Contains(main, "us -> [k, fs, es, d, us + ds]") {
			t.Errorf("%s: an instance stages a whole output:\n%s", c.fixture, main)
		}
	}
}
