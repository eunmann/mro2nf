package emit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

func hasMessage(ds []Diagnostic, sev Severity, substr string) bool {
	for _, d := range ds {
		if d.Severity == sev && strings.Contains(d.Message, substr) {
			return true
		}
	}

	return false
}

// TestDiagnoseWrapsWarnings checks the existing divergence warnings flow through
// Diagnose as Warn diagnostics (a `local` call is a documented no-op).
func TestDiagnoseWrapsWarnings(t *testing.T) {
	prog := &ir.Program{
		Pipelines: map[string]*ir.Pipeline{
			"P": {Name: "P", Calls: []ir.Call{{Name: "C", Callable: "S", Local: true}}},
		},
	}

	if ds := Diagnose(prog, Options{}); !hasMessage(ds, SevWarn, "`local` modifier ignored") {
		t.Errorf("Diagnose must surface the local-ignored warning, got %+v", ds)
	}
}

// inlinedSubMRO's SUB inlines into TOP. Its map call splits self.xs, which TOP
// binds to a projection of GEN's output, so the inlined TOP.SUB_SCALE splits
// GEN.c.xs and keeps a FORK resolve under -native, where an un-inlined
// SUB.SCALE would scatter.
const inlinedSubMRO = `
struct Cfg(int[] xs,)
stage GEN(out Cfg c, src py "s/gen",)
stage SCALE(in int v, out int w, src py "s/scale",)
pipeline SUB(in int[] xs, out int[] ys,){
    map call SCALE(v = split self.xs,) using (local = true,)
    return (ys = SCALE.w,)
}
pipeline TOP(out int[] ys,){
    call GEN()
    call SUB(xs = GEN.c.xs,)
    return (ys = SUB.ys,)
}
call TOP()
`

// TestDiagnoseAnalyzesTheEmittedProgram pins #250: with sub-pipeline inlining
// on, as the CLI defaults, Diagnose reports on the calls Emit emits, and it
// leaves the caller's program for Emit to prepare.
func TestDiagnoseAnalyzesTheEmittedProgram(t *testing.T) {
	prog := lowerMRO(t, inlinedSubMRO)
	opts := Options{OutDir: t.TempDir(), Mre: "mre", Shell: "/x/s", MROFile: "pipeline.mro", InlinePipelines: true}

	ds := Diagnose(prog, opts)
	if !hasMessage(ds, SevWarn, "call TOP.SUB_SCALE: `local` modifier ignored") || hasMessage(ds, SevWarn, "call SUB.SCALE") {
		t.Errorf("want the local warning on the inlined call TOP.SUB_SCALE only, got %+v", ds)
	}

	if _, ok := prog.Pipelines["SUB"]; !ok || len(prog.Pipelines["TOP"].Calls) != 2 {
		t.Fatal("Diagnose inlined the caller's program")
	}

	if err := Emit(prog, opts); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	top, err := os.ReadFile(filepath.Join(opts.OutDir, "modules", "pipe_TOP.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(top), "process FORK_3_TOP__SUB_SCALE") {
		t.Errorf("Emit must fork the inlined TOP.SUB_SCALE:\n%s", top)
	}

	if _, err := os.Stat(filepath.Join(opts.OutDir, "modules", "pipe_SUB.nf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Emit must not emit the inlined SUB module (stat: %v)", err)
	}

	nat := Diagnose(lowerMRO(t, inlinedSubMRO), Options{Native: true, InlinePipelines: true})
	if !hasMessage(nat, SevInfo, "map call TOP.SUB_SCALE keeps one FORK resolve task") {
		t.Errorf("-native: want the FORK remainder info on the inlined TOP.SUB_SCALE, got %+v", nat)
	}
}

// TestHasError checks the abort predicate the CLI gates on.
func TestHasError(t *testing.T) {
	if HasError(nil) {
		t.Error("nil diagnostics: want no error")
	}

	if HasError([]Diagnostic{{Severity: SevWarn}, {Severity: SevInfo}}) {
		t.Error("warn+info only: want no error")
	}

	if !HasError([]Diagnostic{{Severity: SevInfo}, {Severity: SevError, Message: "x"}}) {
		t.Error("an error diagnostic must be detected")
	}
}

// TestDiagnoseNativeHealthOmics checks the -native + -target healthomics
// trade-off is surfaced as an Info (#116): the parameter template declares
// entry inputs that were baked at transpile time, so supplying one fails the
// run. Either flag alone emits no such diagnostic, an entry pipeline with no
// input parameters declares nothing baked (so no Info), and an unparseable
// target emits no Info because Emit rejects it outright.
func TestDiagnoseNativeHealthOmics(t *testing.T) {
	prog := lowerFixture(t, "fork_min")

	on := Diagnose(prog, Options{Native: true, Target: TargetHealthOmics})
	if !hasMessage(on, SevInfo, "-native with -target healthomics") {
		t.Errorf("native + healthomics: want a baked-entry-params info, got %+v", on)
	}

	if got := Diagnose(prog, Options{Native: true}); hasMessage(got, SevInfo, "healthomics") {
		t.Errorf("native without the healthomics target: want no target diagnostic, got %+v", got)
	}

	if got := Diagnose(prog, Options{Target: TargetHealthOmics}); hasMessage(got, SevInfo, "healthomics") {
		t.Errorf("healthomics without -native: want no target diagnostic, got %+v", got)
	}

	noIn := &ir.Program{
		Entry:     &ir.EntryCall{Callable: "P"},
		Pipelines: map[string]*ir.Pipeline{"P": {Name: "P"}},
	}
	if got := Diagnose(noIn, Options{Native: true, Target: TargetHealthOmics}); hasMessage(got, SevInfo, "healthomics") {
		t.Errorf("entry pipeline without inputs: want no target diagnostic, got %+v", got)
	}

	if got := Diagnose(prog, Options{Native: true, Target: "bogus"}); hasMessage(got, SevInfo, "healthomics") {
		t.Errorf("unparseable target: want no target diagnostic (Emit rejects it), got %+v", got)
	}
}

// TestDiagnoseNativeMapped pins the -native map-call remainder messages (#99):
// a file-bearing leaf scatter keeps ONE FORK resolve task and folds its MERGE
// (map_file_split); a sub-pipeline map target keeps FORK and MERGE (map_pipe).
// A fully-collapsed value-only scatter emits NO map diagnostic (fork_min).
func TestDiagnoseNativeMapped(t *testing.T) {
	fbs := Diagnose(lowerFixture(t, "map_file_split"), Options{Native: true})
	if !hasMessage(fbs, SevInfo, "keeps one FORK resolve task") {
		t.Errorf("file-bearing scatter: want a folded-FORK info, got %+v", fbs)
	}

	// A sub-pipeline map folds its outer MERGE and its keyed leaf binds (#99),
	// so only the FORK resolve remains at the outer level.
	mp := Diagnose(lowerFixture(t, "map_pipe"), Options{Native: true})
	if !hasMessage(mp, SevInfo, "keeps one FORK resolve task; its sub-pipeline body runs keyed") {
		t.Errorf("sub-pipeline map: want a folded-FORK + keyed-body info, got %+v", mp)
	}

	// A DISABLED mapped call keeps its MERGE (the skip branch needs it as the
	// null-mix point), so both the FORK and MERGE tasks remain.
	dm := Diagnose(lowerFixture(t, "disabled_map"), Options{Native: true})
	if !hasMessage(dm, SevInfo, "keeps the FORK and MERGE tasks") {
		t.Errorf("disabled mapped call: want a FORK+MERGE info, got %+v", dm)
	}

	fm := Diagnose(lowerFixture(t, "fork_min"), Options{Native: true})
	if hasMessage(fm, SevInfo, "keeps") {
		t.Errorf("fork_min fully collapses under -native: want no map remainder info, got %+v", fm)
	}
}

// TestDiagnoseNativeKeyedScatter pins the keyed-layer wording for a nested map
// call that scatters natively when its pipeline runs plain (#99): a value-only
// inner map keeps only the data-proportional MERGE_K gather (map_pipe_nested),
// while a keyed-ineligible one — here a file-bearing callee input — keeps both
// FORK_K and MERGE_K (map_pipe_nested_file). A DISABLED inner map never plans
// the native scatter (kindMapped), so it reports the plain FORK/MERGE
// remainder instead of the keyed-scatter wording (map_pipe_disabled_nested).
func TestDiagnoseNativeKeyedScatter(t *testing.T) {
	el := Diagnose(lowerFixture(t, "map_pipe_nested"), Options{Native: true})
	if !hasMessage(el, SevInfo, "map call INNER.DBL scatters only when INNER runs plain; under an outer map call its keyed layer keeps the data-proportional MERGE_K gather") {
		t.Errorf("value-only nested map: want the MERGE_K-only keyed info, got %+v", el)
	}

	fb := Diagnose(lowerFixture(t, "map_pipe_nested_file"), Options{Native: true})
	if !hasMessage(fb, SevInfo, "map call INNER.DBL scatters only when INNER runs plain; under an outer map call its keyed layer keeps the FORK_K and MERGE_K tasks") {
		t.Errorf("file-bearing nested map: want the FORK_K+MERGE_K keyed info, got %+v", fb)
	}

	dn := Diagnose(lowerFixture(t, "map_pipe_disabled_nested"), Options{Native: true})
	if hasMessage(dn, SevInfo, "keyed layer keeps") {
		t.Errorf("disabled nested map is kindMapped: want no keyed-scatter wording, got %+v", dn)
	}

	if !hasMessage(dn, SevInfo, "map call INNER.DBL keeps the FORK and MERGE tasks") {
		t.Errorf("disabled nested map: want the plain FORK+MERGE remainder info, got %+v", dn)
	}
}

// TestDiagnoseSplitContexts pins that Diagnose analyzes the program Emit plans:
// split per invocation-known context, with the caller's program untouched. S1
// (plain, invocation-known empty) and S2 (disabled, runtime collection) give
// SUB two copies; only S2's copy receives a queue, so only SUB_ctx2.SCALE
// (which also reads a sibling call) keeps its FORK resolve under -native.
func TestDiagnoseSplitContexts(t *testing.T) {
	prog := lowerMRO(t, `
stage GEN(out int[] xs, out int k, src py "s/gen",)
stage SCALE(in int v, in int k, out int w, src py "s/scale",)
pipeline SUB(in int[] xs, out int[] ys,){
    call GEN as PRE()
    map call SCALE(v = split self.xs, k = PRE.k,)
    return (ys = SCALE.w,)
}
pipeline TOP(in int[] empty, in bool off, out int[] a, out int[] b,){
    call GEN()
    call SUB as S1(xs = self.empty,)
    call SUB as S2(xs = GEN.xs,) using (disabled = self.off,)
    return (a = S1.ys, b = S2.ys,)
}
call TOP(empty = [], off = false,)
`)

	ds := Diagnose(prog, Options{Native: true})
	if !hasMessage(ds, SevInfo, "map call SUB_ctx2.SCALE keeps") {
		t.Errorf("S2's queued SUB copy keeps its FORK resolve: want a remainder info, got %+v", ds)
	}

	if hasMessage(ds, SevInfo, "map call SUB.SCALE") {
		t.Errorf("S1's SUB copy scatters natively: want no remainder info, got %+v", ds)
	}

	if _, ok := prog.Pipelines["SUB_ctx2"]; ok {
		t.Error("Diagnose must not split the caller's program")
	}
}
