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

			// A preflight bound to pipeline inputs now gates the rest of the
			// pipeline (it runs first and downstream calls wait — mrp's behavior).
			// One bound to another call's output cannot gate without a cycle, so it
			// runs in DAG order without the early-abort other stages would get.
			if c.Preflight && (c.Mapped || c.Disabled != nil || bindingsRefCall(c.Bindings)) {
				w = append(w, fmt.Sprintf("call %s: `preflight` is bound to a call output (not pipeline inputs), so it runs in DAG order and does not gate other stages early as it would under mrp", q))
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
