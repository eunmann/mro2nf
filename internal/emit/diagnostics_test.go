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
