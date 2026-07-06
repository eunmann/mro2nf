package emit

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
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

// TestNativeScatterableRefKinds pins the #99 split-source widening at the
// predicate: a whole-field self ref and a whole top-level upstream ref both
// scatter (returning the width field and, for the upstream case, the producing
// call); a PROJECTED ref (self.a.b or CALL.a.b — which can navigate a typed
// map) keeps the FORK path.
func TestNativeScatterableRefKinds(t *testing.T) {
	prog := &ir.Program{Stages: map[string]*ir.Stage{"S": {Name: "S"}}}

	mk := func(r *ir.Ref) ir.Call {
		return ir.Call{
			Name: "C", Callable: "S", Mapped: true,
			Bindings: []ir.Binding{{Param: "v", Value: ir.Value{Ref: r}, Split: true}},
		}
	}

	cases := []struct {
		name       string
		ref        *ir.Ref
		wantOK     bool
		field, cal string
	}{
		{"self whole field", &ir.Ref{Kind: ir.RefKindSelf, ID: "vs"}, true, "vs", ""},
		{"upstream whole output", &ir.Ref{Kind: ir.RefKindCall, ID: "GEN", Output: "vs"}, true, "vs", "GEN"},
		{"self projection", &ir.Ref{Kind: ir.RefKindSelf, ID: "cfg", Output: "vs"}, false, "", ""},
		{"upstream projection", &ir.Ref{Kind: ir.RefKindCall, ID: "GEN", Output: "cfg.vs"}, false, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, field, cal, ok := nativeScatterable(mk(tc.ref), prog, false)
			if ok != tc.wantOK || field != tc.field || cal != tc.cal {
				t.Errorf("nativeScatterable = (%q, %q, %v), want (%q, %q, %v)",
					field, cal, ok, tc.field, tc.cal, tc.wantOK)
			}
		})
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

	// Self-only split inside the disabled sub-pipeline: still scatterable on the
	// O(1) element path, but with pipeargs carried IN the element tuple
	// (forkElementsPa) — a ≤1-item pa queue can't broadcast to N instances, so
	// the broadcast layout would zip N forks to one.
	cp := buildPlan(ds, featureSet{native: true}).pipes["SUB"].calls["SCALE"]
	if cp.kind != kindNativeScatter {
		t.Errorf("self-only scatter in a queued pipeline: kind = %d, want kindNativeScatter", cp.kind)
	}
	if !cp.scatterQueuedPa {
		t.Error("a queue-pipeargs scatter must carry pipeargs in the element tuple (scatterQueuedPa)")
	}

	// A ref-bearing map call (fork_ref's factor = MKFAC.factor) is eligible in a
	// value context but must NOT scatter if its pipeline had queue pipeargs.
	fr := lowerFixture(t, "fork_ref")

	c := findCall(fr.Pipelines["SCALE_REF"], "SCALE")
	if c == nil {
		t.Fatal("fork_ref: no SCALE call")
	}

	if _, _, _, ok := nativeScatterable(*c, fr, false); !ok {
		t.Error("ref-bearing scatter in a value context must be eligible")
	}

	if _, _, _, ok := nativeScatterable(*c, fr, true); ok {
		t.Error("ref-bearing scatter in a queue-pipeargs context must keep the FORK path")
	}

	if got := buildPlan(fr, featureSet{native: true}).pipes["SCALE_REF"].calls["SCALE"].kind; got != kindNativeScatter {
		t.Errorf("fork_ref SCALE kind = %d, want kindNativeScatter (entry context is value)", got)
	}
}

// TestBuildPlanMergeFold pins the #76 merge-fold decision: a scatter whose
// sole consumer is the entry return folds (fork_min), a sole MID-PIPELINE
// fused-stage consumer folds (fork_mid), a fan-out keeps the MERGE
// (fork_fanout), and a sole non-entry return consumer folds
// (fork_disabled_sub).
func TestBuildPlanMergeFold(t *testing.T) {
	fm := lowerFixture(t, "fork_min")
	if cp := buildPlan(fm, featureSet{native: true}).pipes["SCALE_ALL"].calls["SCALE"]; !cp.foldMerge {
		t.Error("fork_min SCALE: sole return consumer must fold the MERGE")
	}

	// fork_mid's SCALE folds into the SUMALL fused-stage task (mid-pipeline).
	fmid := lowerFixture(t, "fork_mid")

	pm := buildPlan(fmid, featureSet{native: true}).pipes["MID"]
	if pm.calls["SUMALL"].kind != kindFusedStage {
		t.Fatalf("fork_mid SUMALL kind = %d, want kindFusedStage", pm.calls["SUMALL"].kind)
	}
	if !pm.calls["SCALE"].foldMerge {
		t.Error("fork_mid SCALE: sole fused-stage consumer must fold the MERGE")
	}

	// fork_fanout's SCALE has two consumers (SUMALL + the return): keep MERGE.
	ff := lowerFixture(t, "fork_fanout")

	pf := buildPlan(ff, featureSet{native: true}).pipes["FANOUT"]
	if pf.calls["SCALE"].kind != kindNativeScatter {
		t.Fatalf("fork_fanout SCALE kind = %d, want kindNativeScatter", pf.calls["SCALE"].kind)
	}
	if pf.calls["SCALE"].foldMerge {
		t.Error("fork_fanout SCALE: two consumers must keep the MERGE task")
	}

	// fork_disabled_sub's SCALE folds into SUB's return BIND (single consumer).
	ds := lowerFixture(t, "fork_disabled_sub")
	if cp := buildPlan(ds, featureSet{native: true}).pipes["SUB"].calls["SCALE"]; !cp.foldMerge {
		t.Error("fork_disabled_sub SCALE: sole return consumer must fold the MERGE")
	}
}

// TestGenerateNativeMergeFold pins the folded-merge emission at the two
// consumer shapes: the entry LAYOUT (fork_min — rendered via generateMain) and
// a non-entry return BIND (fork_disabled_sub's SUB module). Both stage the
// per-fork outs under souts_<id>/ plus the keys sidecar and run the identical
// `mre merge` in-task before their bind.
func TestGenerateNativeMergeFold(t *testing.T) {
	prog := lowerFixture(t, "fork_min")
	f := featureSet{native: true}
	g := genCtx{
		entry: "SCALE_ALL", mroFile: "pipeline.mro", mre: "mre", shell: "/x/sh.py",
		features: f, code: map[string]string{"SCALE": "/x/scale"}, plan: buildPlan(prog, f),
	}

	main := generateMain(prog, g)

	for _, want := range []string{
		"path(souts_SCALE, stageAs: 'souts_SCALE/*')",
		"path 'forkkeys_SCALE.json'",
		// -emptynull: fork_min's split source is an entry self ref, so a zero-fork
		// merge yields null (mrp's invocation-known-empty rule, #99).
		`'mre' merge -emptynull -outs 'scaled' -files "\$(ls -1d souts_SCALE/outs__* 2>/dev/null | sort -V | paste -sd, -)" -keysfile forkkeys_SCALE.json -o merged_SCALE -types 'types.json' -callable 'SCALE' -role out`,
		"-inputs SCALE=merged_SCALE",
		".out.ref_SCALE_souts, ",
		".out.ref_SCALE_keys",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q in native main.nf:\n%s", want, main)
		}
	}

	ds := lowerFixture(t, "fork_disabled_sub")
	gd := genCtx{
		entry: "TOP", mroFile: "pipeline.mro", mre: "mre", shell: "/x/sh.py",
		features: f, code: map[string]string{"SCALE": "/x/scale"}, plan: buildPlan(ds, f),
	}

	mod := generatePipeModule(ds.Pipelines["SUB"], ds, gd)
	if strings.Contains(mod, "process MERGE_") {
		t.Errorf("folded merge in SUB must not emit a MERGE process:\n%s", mod)
	}
	if !strings.Contains(mod, "-o merged_SCALE") || !strings.Contains(mod, "-inputs SCALE=merged_SCALE") {
		t.Errorf("SUB's return BIND must run the merge inline:\n%s", mod)
	}
}

// TestSplitValueOnly pins the O(1) element-path eligibility (#99): a scatter
// whose split element type carries no file leaves is value-only (element path);
// a file / file-struct split element keeps the -index path (its element carries
// bundle markers a plain JSON slice can't reproduce).
func TestSplitValueOnly(t *testing.T) {
	structs := map[string]*ir.StructType{
		"Cfg": {Name: "Cfg", Fields: []ir.Param{{Name: "path", BaseType: "file", IsFile: true}}},
	}
	prog := &ir.Program{Structs: structs}

	mk := func(p ir.Param) (ir.Call, *ir.Stage) {
		s := &ir.Stage{Name: "S", In: []ir.Param{p}}
		c := ir.Call{Callable: "S", Bindings: []ir.Binding{{Param: p.Name, Split: true}}}

		return c, s
	}

	cases := []struct {
		name string
		p    ir.Param
		want bool
	}{
		{"float element", ir.Param{Name: "v", BaseType: "float"}, true},
		{"string element", ir.Param{Name: "v", BaseType: "string"}, true},
		{"file element", ir.Param{Name: "v", BaseType: "file", IsFile: true}, false},
		{"struct-of-file element", ir.Param{Name: "v", BaseType: "Cfg"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := mk(tc.p)
			if got := splitValueOnly(c, s, prog); got != tc.want {
				t.Errorf("splitValueOnly(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestGenerateNativeScatter renders fork_min's pipeline module with -native and
// pins the scatter shape: no FORK process, a fused forkbind -index + main
// process whose instance 0 alone writes the keys sidecar, the keys-only
// sentinel branch for an empty collection, driver-side fork enumeration, and —
// with the sole consumer being the entry return — the MERGE folded away into
// LAYOUT, leaving only the per-fork outs/keys channels on the emit path.
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

	if strings.Contains(mod, "process MERGE_") {
		t.Errorf("a folded merge must not emit a MERGE process:\n%s", mod)
	}

	for _, want := range []string{
		"process STAGE_9_SCALE_ALL__SCALE {",
		// fork_min is a value-only (float) self-source scatter → the O(1)
		// per-element path (#99): index 0 computes keys, index >0 assembles
		// from its own base64 element, no whole-collection re-parse.
		"tuple val(key), val(fi), val(element)",
		"-mapmode array -index 0 -o fargs -keysfile forkkeys.mro2nf.json",
		"printf %s '${element}' | base64 -d > element.json",
		// The element branch carries no full-collection flags (forkbind
		// rejects -mapmode and friends alongside -elementfile).
		"-pipeargs pipeargs -elementfile element.json -o fargs",
		"-mapmode array -keysonly -keysfile forkkeys.mro2nf.json",
		"Mro2nf.forkElements(data, 'values', 'array')",
		"path \"outs__${key}\", type: 'dir', emit: outs, optional: true",
		"path 'forkkeys.mro2nf.json', emit: keys, optional: true",
		// The MERGE folds into the entry return's LAYOUT (#76): the workflow
		// exposes the per-fork outs + keys channels for it instead.
		"ch_SCALE_souts = STAGE_9_SCALE_ALL__SCALE.out.outs.toSortedList { a, b -> a.name <=> b.name }",
		"ch_SCALE_keys = STAGE_9_SCALE_ALL__SCALE.out.keys.first()",
		"ref_SCALE_souts = ch_SCALE_souts",
		"ref_SCALE_keys = ch_SCALE_keys",
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

// TestGenerateNativeScatterUpstreamElements pins the upstream O(1) element
// wiring (#99): a value-only split of an UPSTREAM output slices the PRODUCER'S
// value-channel bundle on the driver (ch_GEN, not pipeargs) into per-fork
// elements, and the fused process still stages the producer as the in_GEN
// broadcast for the index-0 keys resolve and broadcast bindings.
func TestGenerateNativeScatterUpstreamElements(t *testing.T) {
	prog := lowerFixture(t, "fork_upstream")
	f := featureSet{native: true}
	g := genCtx{
		entry: "SCALE_UP", mroFile: "pipeline.mro", mre: "mre",
		shell: "/x/martian_shell.py", features: f,
		code: map[string]string{"SCALE": "/x/scale", "GEN": "/x/gen"},
		plan: buildPlan(prog, f),
	}

	mod := generatePipeModule(prog.Pipelines["SCALE_UP"], prog, g)

	for _, want := range []string{
		"scat_SCALE = ch_GEN.flatMap { data, leaves -> Mro2nf.forkElements(data, 'values', 'array')",
		"tuple val(key), val(fi), val(element)",
		"-pipeargs pipeargs -inputs GEN=in_GEN -elementfile element.json -o fargs",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q in upstream native module:\n%s", want, mod)
		}
	}

	if strings.Contains(mod, "forkScatterRef") {
		t.Errorf("value-only upstream scatter must use the element path, not forkScatterRef:\n%s", mod)
	}
}

// TestGenerateKeyedGatherPinsInputOrder guards -resume stability (#123): every
// keyed gather that groups per-fork/per-chunk outputs with groupTuple must sort
// the grouped bundle list by name before handing it to the gather task. The
// grouped ORDER is completion order, and a task's input tuple order is part of
// its -resume cache key — output bytes are already safe (in-task `sort -V`),
// but an arrival-ordered input list would re-execute the gather on replay.
const sortedGather = ".sort { a, b -> a.name <=> b.name }"

func TestGenerateKeyedGatherPinsInputOrder(t *testing.T) {
	f := featureSet{native: true}

	// Keyed nested map on the driver element scatter (_KS): MERGE_K's grouped
	// souts are sorted inside the remainder-join map.
	prog := lowerFixture(t, "map_pipe_nested")
	g := genCtx{
		entry: "OUTER", mroFile: "pipeline.mro", mre: "mre", shell: "/x/sh.py",
		features: f, code: map[string]string{"DBL": "/x/dbl"}, plan: buildPlan(prog, f),
	}

	scatter := generatePipeModule(prog.Pipelines["INNER"], prog, g)
	wantScatter := ".groupTuple(), remainder: true).map { ok, fk, so -> tuple(ok, (so ?: [])" +
		sortedGather + ", fk) }"

	if !strings.Contains(scatter, "mj_DBL = io_DBL.keys.join(io_DBL.outs.map { ck, bdl -> tuple(Mro2nf.outerKey(ck), bdl) }"+wantScatter) {
		t.Errorf("keyed scatter gather must sort its grouped souts by name:\n%s", scatter)
	}

	// Keyed nested map on the FORK_K path (disabled, so not scatterable): same
	// sorted gather.
	dis := lowerFixture(t, "map_pipe_disabled_nested")
	gd := genCtx{
		entry: "OUTER", mroFile: "pipeline.mro", mre: "mre", shell: "/x/sh.py",
		features: f, code: map[string]string{"DBL": "/x/dbl"}, plan: buildPlan(dis, f),
	}

	forked := generatePipeModule(dis.Pipelines["INNER"], dis, gd)
	// Anchored on the FORK_K remainder-join head so the scatter path's
	// identical sorted suffix cannot satisfy this assertion.
	wantFork := "mj_DBL = FORK_5_INNER__DBL_K.out.keys.join(io_DBL.map { ck, bdl -> tuple(Mro2nf.outerKey(ck), bdl) }" +
		wantScatter

	if !strings.Contains(forked, wantFork) {
		t.Errorf("keyed FORK_K gather must sort its grouped souts by name:\n%s", forked)
	}

	// Keyed split triad: JOIN_K's grouped chunk outs are sorted the same way.
	ms := lowerFixture(t, "map_split")
	gm := genCtx{
		entry: "MS", mroFile: "pipeline.mro", mre: "mre", shell: "/x/sh.py",
		features: f, code: map[string]string{"SUMSQ": "/x/sumsq"}, plan: buildPlan(ms, f),
	}

	triad := generateStageModule(ms.Stages["SUMSQ"], gm)
	wantTriad := ".join(SUMSQ_MAIN_K.out.groupTuple(), remainder: true).map { t -> tuple(t[0], t[2], (t[4] ?: [])" +
		sortedGather + ", t[1], (t[3] ?: [])) }"

	if !strings.Contains(triad, wantTriad) {
		t.Errorf("keyed split triad must sort JOIN_K's grouped chunk outs by name:\n%s", triad)
	}

	// No gather may keep the bare arrival-ordered list.
	for name, mod := range map[string]string{"scatter": scatter, "fork_k": forked, "triad": triad} {
		if strings.Contains(mod, "so ?: [], fk") || strings.Contains(mod, "t[4] ?: [], t[1]") {
			t.Errorf("%s module still gathers in arrival order:\n%s", name, mod)
		}
	}
}
