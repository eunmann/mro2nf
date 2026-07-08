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

// TestDiagnoseContainerTag checks a container target is warned when its image is
// pinned by a mutable tag rather than an immutable @sha256: digest, and that a
// digest, a local target, and an empty ref produce no such warning.
func TestDiagnoseContainerTag(t *testing.T) {
	prog := lowerFixture(t, "split_test")

	if ds := Diagnose(prog, Options{Target: TargetHealthOmics, Container: "ecr/img:latest"}); !hasMessage(ds, SevWarn, "mutable tag") {
		t.Errorf("a mutable-tag cloud container must warn, got %+v", ds)
	}

	if ds := Diagnose(prog, Options{Target: TargetHealthOmics, Container: "ecr/img@sha256:abc123"}); hasMessage(ds, SevWarn, "mutable tag") {
		t.Errorf("a digest-pinned container must not warn, got %+v", ds)
	}

	if ds := Diagnose(prog, Options{Target: TargetLocal, Container: "ecr/img:latest"}); hasMessage(ds, SevWarn, "mutable tag") {
		t.Errorf("a non-container target must not warn about image tags, got %+v", ds)
	}

	// The slim data-plane image is checked too.
	batch := Options{Target: TargetAWSBatch, Container: "ecr/img@sha256:a", ContainerDataplane: "ecr/dp:latest"}
	if ds := Diagnose(prog, batch); !hasMessage(ds, SevWarn, "-container-dataplane") {
		t.Errorf("a mutable-tag data-plane image must warn, got %+v", ds)
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

// TestDiagnoseNativeRunner checks -native-runner surfaces what it does NOT
// cover (#83): each exec/comp stage gets a keeps-the-adapter-path Info, a py
// stage gets none, and -monitor adds the in-process enforcement notice only
// alongside the runner flag. Without -native-runner there is no runner
// diagnostic at all. mixed_adapters has one stage per adapter (py/exec/comp).
func TestDiagnoseNativeRunner(t *testing.T) {
	ma := lowerFixture(t, "mixed_adapters")

	on := Diagnose(ma, Options{NativeRunner: true})
	if !hasMessage(on, SevInfo, "stage ADD is a comp stage and keeps the Martian adapter path") {
		t.Errorf("comp stage: want an adapter-path info naming ADD, got %+v", on)
	}

	if !hasMessage(on, SevInfo, "stage DBL is a exec stage and keeps the Martian adapter path") {
		t.Errorf("exec stage: want an adapter-path info naming DBL, got %+v", on)
	}

	if hasMessage(on, SevInfo, "stage GEN") {
		t.Errorf("py stage GEN runs natively: want no adapter-path info for it, got %+v", on)
	}

	if hasMessage(on, SevInfo, "-monitor enforcement") {
		t.Errorf("without -monitor: want no monitor notice, got %+v", on)
	}

	mon := Diagnose(ma, Options{NativeRunner: true, Monitor: true})
	if !hasMessage(mon, SevInfo, "-monitor enforcement runs in-process") {
		t.Errorf("with -monitor: want the in-process enforcement notice, got %+v", mon)
	}

	off := Diagnose(ma, Options{Monitor: true})
	if hasMessage(off, SevInfo, "native-runner") {
		t.Errorf("without -native-runner: want no runner diagnostic, got %+v", off)
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
