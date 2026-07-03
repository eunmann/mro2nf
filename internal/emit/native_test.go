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

// TestBuildPlanNativeScatterQueueContext guards the queue-pipeargs rule: a
// pipeline reached through a disabled call gets a QUEUE channel as pipeargs,
// so a scatter with upstream-ref inputs would zip its N-item fork channel
// against 1-item queues and run a single fork. Self-only scatters stay
// eligible there (the fork channel is the only multi-item input); ref-bearing
// scatters stay eligible only in value-channel contexts.
func TestBuildPlanNativeScatterQueueContext(t *testing.T) {
	ds := lowerFixture(t, "fork_disabled_sub")

	if q := queuePipeArgs(ds); !q["SUB"] || q["TOP"] {
		t.Errorf("queuePipeArgs = %v, want SUB queued (disabled callee), TOP not", q)
	}

	// Self-only split inside the disabled sub-pipeline: still scatterable.
	if got := buildPlan(ds, featureSet{native: true}).pipes["SUB"].calls["SCALE"].kind; got != kindNativeScatter {
		t.Errorf("self-only scatter in a queued pipeline: kind = %d, want kindNativeScatter", got)
	}

	// A ref-bearing map call (fork_ref's factor = MKFAC.factor) is eligible in a
	// value context but must NOT scatter if its pipeline had queue pipeargs.
	fr := lowerFixture(t, "fork_ref")

	c := findCall(fr.Pipelines["SCALE_REF"], "SCALE")
	if c == nil {
		t.Fatal("fork_ref: no SCALE call")
	}

	if _, _, ok := nativeScatterable(*c, fr, false); !ok {
		t.Error("ref-bearing scatter in a value context must be eligible")
	}

	if _, _, ok := nativeScatterable(*c, fr, true); ok {
		t.Error("ref-bearing scatter in a queue-pipeargs context must keep the FORK path")
	}

	if got := buildPlan(fr, featureSet{native: true}).pipes["SCALE_REF"].calls["SCALE"].kind; got != kindNativeScatter {
		t.Errorf("fork_ref SCALE kind = %d, want kindNativeScatter (entry context is value)", got)
	}
}

// TestGenerateNativeScatter renders fork_min's pipeline module with -native and
// pins the scatter shape: no FORK process, a fused forkbind -index + main
// process whose instance 0 alone writes the keys sidecar, the keys-only
// sentinel branch for an empty collection, driver-side fork enumeration, and
// an unchanged MERGE gather fed by the single-producer keys channel.
func TestGenerateNativeScatter(t *testing.T) {
	prog := lowerFixture(t, "fork_min")
	f := featureSet{native: true}
	g := genCtx{
		entry:    "SCALE_ALL",
		mroFile:  "pipeline.mro",
		mre:      "mre",
		shell:    "/x/martian_shell.py",
		features: f,
		code:     map[string]string{"SCALE": "/x/scale"},
		plan:     buildPlan(prog, f),
	}

	mod := generatePipeModule(prog.Pipelines["SCALE_ALL"], prog, g)

	if strings.Contains(mod, "process FORK_") {
		t.Errorf("-native scatter must not emit a FORK process:\n%s", mod)
	}

	for _, want := range []string{
		"process STAGE_9_SCALE_ALL__SCALE {",
		"-mapmode array -index ${fi} -o fargs${fi == 0 ? ' -keysfile forkkeys.json' : ''}",
		"-keysonly -keysfile forkkeys.json",
		"Mro2nf.forkScatter(data, leaves, 'values', 'array')",
		"path \"outs__${key}\", type: 'dir', emit: outs, optional: true",
		"path 'forkkeys.json', emit: keys, optional: true",
		".out.keys.first(), types)",
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
