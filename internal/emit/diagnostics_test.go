package emit

import (
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

// TestDiagnoseFuseChains checks the -fuse-chains trade-off is surfaced: an Info
// noting how many chains fused (with the coarser-resume caveat), and — when the
// flag qualifies nothing — an Info that it had no effect, so the user is not
// misled. Without the flag there is no chain diagnostic.
func TestDiagnoseFuseChains(t *testing.T) {
	chain := lowerFixture(t, "chain_fuse")

	on := Diagnose(chain, Options{FuseChains: true})
	if !hasMessage(on, SevInfo, "fused 1 chain") {
		t.Errorf("chain_fuse with -fuse-chains: want a fused-chain info, got %+v", on)
	}

	if got := Diagnose(chain, Options{}); hasMessage(got, SevInfo, "fuse-chains") {
		t.Errorf("chain_fuse without the flag: want no chain diagnostic, got %+v", got)
	}

	// diamond_min's source (GEN) has two consumers, so nothing qualifies.
	noop := Diagnose(lowerFixture(t, "diamond_min"), Options{FuseChains: true})
	if !hasMessage(noop, SevInfo, "had no effect") {
		t.Errorf("diamond_min with -fuse-chains: want a no-op info, got %+v", noop)
	}
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

// TestDiagnoseFoldDisables checks -fold-disables warns which gate it pruned and
// on which entry input (the override caveat), and only with the flag on.
func TestDiagnoseFoldDisables(t *testing.T) {
	fd := lowerFixture(t, "fold_disable")

	on := Diagnose(fd, Options{FoldDisables: true})
	if !hasMessage(on, SevWarn, "P.GEN pruned") || !hasMessage(on, SevWarn, "self.skip") {
		t.Errorf("want a fold warning naming P.GEN and self.skip, got %+v", on)
	}

	if got := Diagnose(fd, Options{}); hasMessage(got, SevWarn, "pruned") {
		t.Errorf("no fold warning without the flag, got %+v", got)
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
