package emit

import (
	"fmt"

	"github.com/eunmann/mro2nf/internal/ir"
)

// Severity ranks a Diagnostic. An Error aborts the transpile; Warn and Info are
// printed and the transpile continues.
type Severity int

const (
	// SevInfo notes a trade-off the user opted into (e.g. coarser -resume).
	SevInfo Severity = iota
	// SevWarn flags a documented divergence from mrp that still produces output.
	SevWarn
	// SevError marks a flag/pipeline combination that would emit a wrong or broken
	// project — the transpile must not proceed.
	SevError
)

// Diagnostic is one pre-emit finding about a program under a set of options.
type Diagnostic struct {
	Severity Severity
	Message  string
}

// Diagnose runs the pre-emit checks for a program under opts: the unconditional
// divergence warnings (preflight/local/volatile) plus flag-conditional
// diagnostics that catch an opt-in flag being unsafe or a no-op for this
// pipeline. Callers print the results and abort if HasError reports an Error —
// the seam #72/#76 add their flag-specific error checks to.
func Diagnose(prog *ir.Program, opts Options) []Diagnostic {
	f := featureSet{fuseChains: opts.FuseChains}
	pl := buildPlan(prog, f)

	warns := Warnings(prog)
	ds := make([]Diagnostic, 0, len(warns)+1)

	for _, w := range warns {
		ds = append(ds, Diagnostic{Severity: SevWarn, Message: w})
	}

	return append(ds, chainDiagnostics(f, pl)...)
}

// HasError reports whether any diagnostic is fatal.
func HasError(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == SevError {
			return true
		}
	}

	return false
}

// chainDiagnostics reports the -fuse-chains trade-off: how many chains fuse (with
// the coarser-resume caveat), or that the flag had no effect so the user is not
// misled into thinking it did something.
func chainDiagnostics(f featureSet, pl emitPlan) []Diagnostic {
	if !f.fuseChains {
		return nil
	}

	fused := 0

	for _, pp := range pl.pipes {
		for _, cp := range pp.calls {
			if cp.kind == kindFusedChain {
				fused++
			}
		}
	}

	if fused == 0 {
		return []Diagnostic{{
			Severity: SevInfo,
			Message:  "-fuse-chains had no effect: no single-consumer, equal-resource source stage qualified",
		}}
	}

	return []Diagnostic{{
		Severity: SevInfo,
		Message:  fmt.Sprintf("-fuse-chains fused %d chain(s); -resume and per-stage retry granularity is coarser for those stages", fused),
	}}
}
