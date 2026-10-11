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
