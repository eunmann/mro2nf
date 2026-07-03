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
	f := featureSet{fuseChains: opts.FuseChains, foldDisables: opts.FoldDisables}
	pl := buildPlan(prog, f)

	warns := Warnings(prog)
	ds := make([]Diagnostic, 0, len(warns)+1)

	for _, w := range warns {
		ds = append(ds, Diagnostic{Severity: SevWarn, Message: w})
	}

	ds = append(ds, chainDiagnostics(f, pl)...)

	return append(ds, foldDiagnostics(prog, f, pl)...)
}

// foldDiagnostics warns which disable gates -fold-disables pruned and on which
// entry input, so the user knows overriding that input at run time will not
// re-enable the pruned stage (the safety trade the flag opts into).
func foldDiagnostics(prog *ir.Program, f featureSet, pl emitPlan) []Diagnostic {
	if !f.foldDisables {
		return nil
	}

	var ds []Diagnostic

	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		for _, c := range p.Calls {
			if pl.pipes[name].calls[c.Name].kind != kindFoldedOff {
				continue
			}

			input, _ := foldDisableOff(prog, p, c)
			ds = append(ds, Diagnostic{
				Severity: SevWarn,
				Message: fmt.Sprintf("call %s.%s pruned: disabled=self.%s folds to true; overriding %q at run time will NOT re-enable it",
					name, c.Name, input, input),
			})
		}
	}

	if len(ds) == 0 {
		return []Diagnostic{{
			Severity: SevInfo,
			Message:  "-fold-disables had no effect: no entry-determinable always-disabled stage to prune",
		}}
	}

	return ds
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
