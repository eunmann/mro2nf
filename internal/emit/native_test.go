package emit

import (
	"strings"
	"testing"
)

// TestBuildPlanNativeScatter guards #76's scatter eligibility: with -native an
// eligible map call (non-split leaf stage callee, no disable gate, exactly one
// split binding that is a whole-field self ref) becomes kindNativeScatter with
// the split field recorded; without the flag it stays kindMapped.
func TestBuildPlanNativeScatter(t *testing.T) {
	fm := lowerFixture(t, "fork_min")

	if got := buildPlan(fm, featureSet{}).pipes["SCALE_ALL"].calls["SCALE"].kind; got != kindMapped {
		t.Errorf("default: SCALE kind = %d, want kindMapped", got)
	}

	pl := buildPlan(fm, featureSet{native: true})

	cp := pl.pipes["SCALE_ALL"].calls["SCALE"]
	if cp.kind != kindNativeScatter || cp.scatterField != "values" {
		t.Errorf("native: SCALE kind = %d, field = %q; want kindNativeScatter over \"values\"",
			cp.kind, cp.scatterField)
	}

	// The scattered stage runs inline in the fused process, so its keyed variant
	// and (nothing else referencing SCALE) its whole module are dead.
	if pl.keyed["SCALE"] {
		t.Error("native scatter must not mark the callee keyed")
	}

	if pl.modules["SCALE"] {
		t.Error("native scatter leaves no reference to the stage module")
	}
}

// TestBuildPlanNativeScatterIneligible pins the shapes that must KEEP the FORK
// path under -native: zipped multi-split bindings, an upstream-ref split
// source, a disable gate, a sub-pipeline callee, and a split-stage callee.
// Each needs the FORK task's full bind resolution (or keyed fan-out), so
// scattering them would be wrong — not just unoptimized.
func TestBuildPlanNativeScatterIneligible(t *testing.T) {
	cases := []struct{ fixture, pipe, call, why string }{
		{"multisplit", "MS", "PAIR", "two zipped split bindings"},
		{"map_null_map", "P", "SCALE", "split source is an upstream call output"},
		{"disabled_map", "Q", "DBL", "disable gate"},
		{"map_pipe", "OUTER", "INNER", "sub-pipeline callee"},
		{"map_split", "MS", "SUMSQ", "split-stage callee"},
	}

	for _, tc := range cases {
		prog := lowerFixture(t, tc.fixture)

		if got := buildPlan(prog, featureSet{native: true}).pipes[tc.pipe].calls[tc.call].kind; got != kindMapped {
			t.Errorf("%s (%s): %s.%s kind = %d, want kindMapped", tc.fixture, tc.why, tc.pipe, tc.call, got)
		}
	}
}

// TestGenerateNativeScatter renders fork_min's pipeline module with -native and
// pins the scatter shape: no FORK process, a fused forkbind -index + main
// process, driver-side fork enumeration, the keys fallback to the empty-fork
// asset, and an unchanged MERGE gather.
func TestGenerateNativeScatter(t *testing.T) {
	prog := lowerFixture(t, "fork_min")
	f := featureSet{native: true}
	g := genCtx{
		entry:    "SCALE_ALL",
		mroFile:  "pipeline.mro",
		mre:      "mre",
		shell:    "/x/martian_shell.py",
		features: f,
		native:   true,
		code:     map[string]string{"SCALE": "/x/scale"},
		plan:     buildPlan(prog, f),
	}

	mod := generatePipeModule(prog.Pipelines["SCALE_ALL"], prog, g)

	if strings.Contains(mod, "process FORK_") {
		t.Errorf("-native scatter must not emit a FORK process:\n%s", mod)
	}

	for _, want := range []string{
		"forkbind -spec 'spec.json' -pipeargs pipeargs -mapmode array -index ${fi} -o fargs -keysfile forkkeys.json",
		"Mro2nf.forkScatter(data, leaves, 'values')",
		"process STAGE_9_SCALE_ALL__SCALE {",
		"tuple val(key), path(\"outs__${key}\", type: 'dir'), emit: outs",
		"concat(Channel.of(file(\"${projectDir}/_assets/forkkeys_array.json\"))).first()",
		"process MERGE_9_SCALE_ALL__SCALE {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q in native module:\n%s", want, mod)
		}
	}

	// Default mode for the same program is untouched: FORK + keyed callee.
	gd := genCtx{entry: "SCALE_ALL", mre: "mre", code: g.code, plan: buildPlan(prog, featureSet{})}

	def := generatePipeModule(prog.Pipelines["SCALE_ALL"], prog, gd)
	if !strings.Contains(def, "process FORK_9_SCALE_ALL__SCALE") {
		t.Errorf("default mode must keep the FORK process:\n%s", def)
	}
}
