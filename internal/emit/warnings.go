package emit

import (
	"fmt"

	"github.com/eunmann/mro2nf/internal/ir"
)

// Warnings returns human-readable notices for Martian constructs the generated
// project reproduces with a documented divergence from mrp. These do NOT change
// output correctness (the pipeline still computes the same results), but the
// operator should know the runtime behavior differs — so the mro2nf CLI logs them
// at transpile time rather than letting the difference pass silently.
//
// Constructs with no faithful lowering at all (unknown expressions/adapters, a
// nested typed-map field projection, a comp stage without mrjob) are hard
// errors elsewhere — those fail the transpile; these only warn.
func Warnings(prog *ir.Program) []string {
	var w []string

	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		for _, c := range p.Calls {
			q := name + "." + c.Name

			// Only a plain preflight bound solely to pipeline inputs gates the
			// rest of the pipeline (it runs first and downstream calls wait —
			// mrp's behavior). Anything else — a mapped or disabled preflight, or
			// one bound to another call's output (which cannot gate without a
			// cycle) — runs in DAG order without the early abort, so name the
			// actual trigger.
			if reason := preflightUngateable(c); reason != "" {
				w = append(w, fmt.Sprintf("call %s: `preflight` %s, so it runs in DAG order and does not gate other stages early as it would under mrp", q, reason))
			}

			if c.Local {
				w = append(w, fmt.Sprintf("call %s: `local` modifier ignored — Nextflow schedules it like any other task", q))
			}

			if c.Volatile {
				w = append(w, fmt.Sprintf("call %s: `volatile` ignored — there is no mid-run VDR deletion; the work directory is retained (higher peak disk, identical outputs)", q))
			}
		}
	}

	return w
}

// preflightUngateable returns why a preflight call cannot act as mrp's early
// gate, or "" for a gateable (or non-preflight) call. It is the single
// definition of the gate condition: partitionGateablePreflight wires exactly
// the calls this reports "" for, so the warning and the emitted gating cannot
// drift apart.
func preflightUngateable(c ir.Call) string {
	switch {
	case !c.Preflight:
		return ""
	case c.Mapped:
		return "is a map call"
	case c.Disabled != nil:
		return "carries a `disabled` gate"
	case bindingsRefCall(c.Bindings):
		return "is bound to a call output (not pipeline inputs)"
	default:
		return ""
	}
}
