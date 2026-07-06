package emit

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestGenDisabledWiringTaskBranch pins genDisabledWiring's DISABLE-task branch,
// which no committed fixture reaches (every fixture's plain-layer disable flag
// is a single top-level field, so nativeDisableGate folds the task away): a
// PROJECTED disable ref (CFG.cfg.flag — a nested output path) is not
// driver-gateable, so needsDisableTask forces the standalone DISABLE bind and
// the run/skip branch joins on its output. The exact-text pin machine-checks
// the template's interpolations (call name, BIND, DISABLE, callee, null
// bundle) in their rendered positions.
func TestGenDisabledWiringTaskBranch(t *testing.T) {
	prog := lowerMRO(t, `
struct Cfg(bool flag,)
stage CFG(out Cfg cfg, src py "s/cfg",)
stage WORK(in int x, out int y, src py "s/work",)
pipeline P(in int x, out int y,){
    call CFG()
    call WORK(x = self.x,) using (disabled = CFG.cfg.flag,)
    return (y = WORK.y,)
}
call P(x = 1,)
`)

	p := prog.Pipelines["P"]

	c, ok := callByName(p, "WORK")
	if !ok {
		t.Fatalf("no WORK call in pipeline P")
	}

	if src, field, native := nativeDisableGate(c); native {
		t.Fatalf("projected disable ref: nativeDisableGate = (%q, %q, true), want the DISABLE-task fallback", src, field)
	}

	cp := buildPlan(prog, featureSet{}).pipes["P"].calls["WORK"]
	if cp.kind != kindPlainBind || !cp.disableTask {
		t.Fatalf("projected-disable leaf: kind = %d, disableTask = %t; want kindPlainBind with a DISABLE task",
			cp.kind, cp.disableTask)
	}

	var b strings.Builder

	genDisabledWiring(&b, p.Name, c, callAlias(p.Name, c.Name))

	want := `    DISABLE_1_P__WORK(pa, ch_CFG, types, file("${projectDir}/_assets/bindspecs/DISABLE_1_P__WORK.json"))
    g_WORK = BIND_1_P__WORK.out.combine(DISABLE_1_P__WORK.out).branch { data, leaves, d ->
        def off = Mro2nf.disabled(d)
        run: !off
        skip: off
    }
    r_WORK = wf_1_P__WORK(g_WORK.run.map { data, leaves, d -> tuple(data, leaves) })
    s_WORK = g_WORK.skip.map { data, leaves, d -> tuple(file("${projectDir}/nulls/1_P__WORK/data.json"), []) }
    ch_WORK = r_WORK.mix(s_WORK).first()
`
	if diff := cmp.Diff(want, b.String()); diff != "" {
		t.Errorf("DISABLE-task wiring mismatch (-want +got):\n%s", diff)
	}
}
