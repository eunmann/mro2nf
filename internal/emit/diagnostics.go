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
	f := opts.featureSet()
	pl := buildPlan(prog, f)

	warns := Warnings(prog)
	ds := make([]Diagnostic, 0, len(warns)+1)

	for _, w := range warns {
		ds = append(ds, Diagnostic{Severity: SevWarn, Message: w})
	}

	ds = append(ds, chainDiagnostics(f, pl)...)
	ds = append(ds, nativeDiagnostics(prog, f, pl)...)
	ds = append(ds, nativeTargetDiagnostics(prog, f, opts.Target)...)
	ds = append(ds, runnerDiagnostics(prog, f, opts.Monitor)...)

	return append(ds, foldDiagnostics(prog, f, pl)...)
}

// nativeDiagnostics surfaces which map calls -native could NOT fully collapse,
// so partial collapse is visible up front instead of discovered by diffing the
// generated project (#83's detect-and-refuse principle: an opt-in never
// silently under-delivers). The message names exactly what orchestration
// remains, which the value-only element scatter (#99) eliminates entirely: a
// kindMapped call keeps ONE FORK resolve task (O(total), not per-instance) plus
// a MERGE only when the gather cannot fold into its consumer; a scatter reached
// through an outer map call runs its keyed layer instead of collapsing.
func nativeDiagnostics(prog *ir.Program, f featureSet, pl emitPlan) []Diagnostic {
	if !f.native {
		return nil
	}

	var ds []Diagnostic

	for _, name := range sortedKeys(prog.Pipelines) {
		for _, c := range prog.Pipelines[name].Calls {
			if !c.Mapped {
				continue
			}

			cp := pl.pipes[name].calls[c.Name]

			if cp.kind == kindMapped {
				ds = append(ds, Diagnostic{Severity: SevInfo, Message: mappedRemainderMsg(prog, name, c, cp)})
			} else if cp.kind == kindNativeScatter && pl.keyed[name] {
				// kindNativeScatter implies c.Mapped, so keyedKind==keyedScatter
				// here is exactly the old keyedScatterable(c) consult; a future
				// non-mapped scatter shape would break that equivalence.
				// Under an outer map call a value-only inner map collapses its
				// per-outer-fork FORK_K resolve into a driver element scatter
				// (#99), keeping only the data-proportional MERGE_K gather; a
				// file-bearing/multi-split/disabled inner map keeps both.
				kept := "the FORK_K and MERGE_K tasks"
				if cp.keyedKind == keyedScatter {
					kept = "the data-proportional MERGE_K gather (its inner fork resolve is a driver element scatter)"
				}

				ds = append(ds, Diagnostic{
					Severity: SevInfo,
					Message: fmt.Sprintf("-native: map call %s.%s scatters only when %s runs plain; under an outer map call its keyed layer keeps %s",
						name, c.Name, name, kept),
				})
			}
		}
	}

	return ds
}

// mappedRemainderMsg describes the orchestration a kindMapped call keeps and
// why it is not on the O(1) element scatter path.
func mappedRemainderMsg(prog *ir.Program, pipeline string, c ir.Call, cp callPlan) string {
	tasks := "one FORK resolve task"
	if !cp.foldMerge {
		tasks = "the FORK and MERGE tasks"
	}

	switch s, isStage := prog.Stages[c.Callable]; {
	case !isStage:
		// A sub-pipeline callee runs its keyed body per outer fork. Its leaf
		// calls are fused (#99), so only a nested map (FORK_K/MERGE_K), a
		// disabled call, or a split call inside keeps a keyed bookkeeping task.
		tasks += "; its sub-pipeline body runs keyed per outer fork (leaf calls fused; a nested-map, disabled, or split call inside keeps its keyed task)"
	case s.Split:
		// A split-stage callee also fans out the intrinsic per-fork split triad
		// (SPLIT/MAIN/JOIN) — that is genuine compute matching mrp's jobs 1:1,
		// not orchestration overhead, but name it so the task count is expected.
		tasks += " (plus the intrinsic per-fork split triad, matching mrp 1:1)"
	}

	return fmt.Sprintf("-native: map call %s.%s keeps %s (not on the O(1) element scatter: needs a single whole-field self/upstream split feeding a value-typed param of an undisabled, non-preflight leaf stage; a file-typed, projected, or multi-split element and split-stage / sub-pipeline callees all keep the FORK resolve)",
		pipeline, c.Name, tasks)
}

// nativeTargetDiagnostics surfaces the -native + -target healthomics trade-off
// (#116): entry inputs stay declared in parameter-template.json (HealthOmics
// rejects a request carrying an undeclared parameter, so dropping them would
// change which requests fail), but their values were baked at transpile time
// and supplying one fails the run at launch.
func nativeTargetDiagnostics(prog *ir.Program, f featureSet, rawTarget Target) []Diagnostic {
	// Normalize exactly as Emit does, so a direct Diagnose caller passing a
	// non-canonical target string agrees with Emit. On a ParseTarget error emit
	// no target-Info: Emit rejects that target outright.
	target, err := ParseTarget(string(rawTarget))
	if err != nil || !f.native || target != TargetHealthOmics {
		return nil
	}

	// No entry inputs means the template declares no baked parameters
	// (entryInParams is the same enumeration writeHealthOmicsPackaging walks),
	// so there is no trade-off to surface.
	if prog.Entry == nil || len(entryInParams(prog)) == 0 {
		return nil
	}

	return []Diagnostic{{
		Severity: SevInfo,
		Message:  "-native with -target healthomics: entry inputs are baked at transpile time; parameter-template.json still declares them (HealthOmics rejects undeclared parameters) but supplying one fails the run — re-transpile to change entry values",
	}}
}

// runnerDiagnostics surfaces what -native-runner does NOT cover, so the opt-in
// never silently under-delivers (#83): comp/exec stages keep the Martian
// adapter path, and -monitor's RSS enforcement runs in-process (the runner's
// watchdog) rather than mre's process-group monitor.
func runnerDiagnostics(prog *ir.Program, f featureSet, monitor bool) []Diagnostic {
	if !f.nativeRunner {
		return nil
	}

	var ds []Diagnostic

	for _, name := range sortedKeys(prog.Stages) {
		if lang := prog.Stages[name].Lang; lang != ir.LangPy {
			ds = append(ds, Diagnostic{
				Severity: SevInfo,
				Message:  fmt.Sprintf("-native-runner: stage %s is a %s stage and keeps the Martian adapter path", name, lang),
			})
		}
	}

	if monitor {
		ds = append(ds, Diagnostic{
			Severity: SevInfo,
			Message:  "-native-runner: -monitor enforcement runs in-process (RLIMIT_AS vmem cap + RSS watchdog), not mre's process-group monitor",
		})
	}

	return ds
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
