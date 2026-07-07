package emit

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestGenDisabledWiringNestedProjection pins #209: a PROJECTED disable ref
// (CFG.cfg.flag — a nested output path) is read natively on the driver, NOT via a
// standalone DISABLE task. A valid disable flag is always a scalar bool, so the
// projection is pure struct navigation the driver walks with Mro2nf.disabledField
// over the upstream channel's sidecar. The exact-text pin machine-checks the
// native branch's interpolations (call name, BIND, source channel, dotted field
// path, callee, null bundle) — and that no DISABLE process is emitted.
func TestGenDisabledWiringNestedProjection(t *testing.T) {
	// A disabled call to a SUB-PIPELINE (can't fuse bind+main into one task, so it
	// stays kindPlainBind and uses genDisabledWiring) — this is CellRanger's shape
	// (e.g. DISABLE_..__COUNT_ANALYZER gates a sub-pipeline on config.disable_count).
	prog := lowerMRO(t, `
struct Cfg(bool flag,)
stage CFG(out Cfg cfg, src py "s/cfg",)
stage LEAF(in int x, out int y, src py "s/leaf",)
pipeline SUB(in int x, out int y,){
    call LEAF(x = self.x,)
    return (y = LEAF.y,)
}
pipeline P(in int x, out int y,){
    call CFG()
    call SUB(x = self.x,) using (disabled = CFG.cfg.flag,)
    return (y = SUB.y,)
}
call P(x = 1,)
`)

	p := prog.Pipelines["P"]

	c, ok := callByName(p, "SUB")
	if !ok {
		t.Fatalf("no SUB call in pipeline P")
	}

	src, field, native := nativeDisableGate(c)
	if !native || src != "ch_CFG" || field != "cfg.flag" {
		t.Fatalf("nested projected disable ref: nativeDisableGate = (%q, %q, %t), want (ch_CFG, cfg.flag, true)", src, field, native)
	}

	cp := buildPlan(prog, featureSet{}).pipes["P"].calls["SUB"]
	if cp.kind != kindPlainBind || cp.disableTask {
		t.Fatalf("nested-projected-disabled sub-pipeline: kind = %d, disableTask = %t; want kindPlainBind with NO DISABLE task",
			cp.kind, cp.disableTask)
	}

	var b strings.Builder

	genDisabledWiring(&b, p.Name, c, callAlias(p.Name, c.Name))

	want := `    g_SUB = BIND_1_P__SUB.out.combine(ch_CFG).branch { data, leaves, gd, gl ->
        def off = Mro2nf.disabledField(gd, 'cfg.flag')
        run: !off
        skip: off
    }
    r_SUB = wf_1_P__SUB(g_SUB.run.map { data, leaves, gd, gl -> tuple(data, leaves) })
    s_SUB = g_SUB.skip.map { data, leaves, gd, gl -> tuple(file("${projectDir}/nulls/1_P__SUB/data.json"), []) }
    ch_SUB = r_SUB.mix(s_SUB).first()
`
	if diff := cmp.Diff(want, b.String()); diff != "" {
		t.Errorf("native nested-disable wiring mismatch (-want +got):\n%s", diff)
	}

	if strings.Contains(b.String(), "DISABLE_") {
		t.Errorf("nested projected disable must not emit a DISABLE process:\n%s", b.String())
	}
}

// TestFusedDisabledEmitsNoDeadInclude guards #187: a natively-gated disabled call
// fuses bind+main into a self-contained process (genFusedDisabledWiring uses the
// fused process, not the wf_ alias), so genPipeIncludes must not emit a dead
// `include { wf_<stage> as <call> }` for it (which kept the module alive and
// defeated #82 pruning).
func TestFusedDisabledEmitsNoDeadInclude(t *testing.T) {
	prog := lowerMRO(t, `
stage CFG(out bool flag, src py "s/cfg",)
stage WORK(in int x, out int y, src py "s/work",)
pipeline P(in int x, out int y,){
    call CFG()
    call WORK(x = self.x,) using (disabled = CFG.flag,)
    return (y = WORK.y,)
}
call P(x = 1,)
`)

	g := genCtx{plan: buildPlan(prog, featureSet{})}

	cp := g.plan.pipes["P"].calls["WORK"]
	if cp.kind != kindFusedDisabled {
		t.Fatalf("WORK: kind = %d, want kindFusedDisabled (a direct bool disable ref)", cp.kind)
	}

	var b strings.Builder
	genPipeIncludes(&b, prog.Pipelines["P"], prog, g)

	if alias := callAlias("P", "WORK"); strings.Contains(b.String(), alias) {
		t.Errorf("fused-disabled WORK must not emit a dead wf_ include (%s); got:\n%s", alias, b.String())
	}
}

// TestFusedDisabledSplit pins the disabled-SPLIT fold: a natively-gated
// (self.<flag>) disabled SPLIT-stage call folds its bind into the fused SPLIT
// task (kindFusedDisabledSplit) — no standalone BIND — while an ENABLED split
// stays kindFusedSplit. The wiring gates pa into the fused split workflow and
// mixes the null bundle on the skip branch; the includes alias the stage's
// MAIN/JOIN (not a dead wf_ import).
func TestFusedDisabledSplit(t *testing.T) {
	prog := lowerMRO(t, `
stage SP(in int v, out int w, src py "s/sp",) split using (in int c, out int cw,)
pipeline P(in int v, in bool skip, out int a, out int b,){
    call SP as EN(v = self.v,)
    call SP as DIS(v = self.v,) using (disabled = self.skip,)
    return (a = EN.w, b = DIS.w,)
}
call P(v = 1, skip = false,)
`)

	g := genCtx{plan: buildPlan(prog, featureSet{})}
	calls := g.plan.pipes["P"].calls

	if calls["EN"].kind != kindFusedSplit {
		t.Fatalf("EN: kind = %d, want kindFusedSplit", calls["EN"].kind)
	}
	if calls["DIS"].kind != kindFusedDisabledSplit {
		t.Fatalf("DIS: kind = %d, want kindFusedDisabledSplit", calls["DIS"].kind)
	}

	p := prog.Pipelines["P"]
	dis, _ := callByName(p, "DIS")

	var b strings.Builder
	genFusedDisabledSplitWiring(&b, p, dis)

	want := `    g_DIS = pa.branch { data, leaves ->
        def off = Mro2nf.disabledField(data, 'skip')
        run: !off
        skip: off
    }
    r_DIS = STAGE_1_P__DIS(g_DIS.run)
    s_DIS = g_DIS.skip.map { data, leaves -> tuple(file("${projectDir}/nulls/1_P__DIS/data.json"), []) }
    ch_DIS = r_DIS.mix(s_DIS).first()
`
	if diff := cmp.Diff(want, b.String()); diff != "" {
		t.Errorf("fused-disabled-split wiring mismatch (-want +got):\n%s", diff)
	}

	if strings.Contains(b.String(), "BIND_") || strings.Contains(b.String(), "DISABLE_") {
		t.Errorf("fused-disabled split must emit no standalone BIND/DISABLE:\n%s", b.String())
	}

	// The includes alias the stage's MAIN/JOIN (fused split needs them), not the
	// plain wf_ callee alias.
	var inc strings.Builder
	genPipeIncludes(&inc, p, prog, g)
	if !strings.Contains(inc.String(), "SP_MAIN as "+fusedMainAlias("P", "DIS")) {
		t.Errorf("disabled split must import the aliased MAIN/JOIN; got:\n%s", inc.String())
	}
	if alias := callAlias("P", "DIS"); strings.Contains(inc.String(), alias) {
		t.Errorf("disabled split must not emit a dead wf_ include (%s); got:\n%s", alias, inc.String())
	}
}
