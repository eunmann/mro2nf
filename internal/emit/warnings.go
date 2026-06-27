package emit

import (
	"fmt"

	"github.com/eunmann/martian-nextflow/internal/ir"
)

// Warnings returns human-readable notices for Martian constructs the generated
// project reproduces with a documented divergence from mrp. These do NOT change
// output correctness (the pipeline still computes the same results), but the
// operator should know the runtime behavior differs — so the mart CLI logs them
// at transpile time rather than letting the difference pass silently.
//
// Constructs with no faithful lowering at all (unknown expressions/adapters, an
// array-of-map<S> field projection, a comp stage without mrjob) are hard errors
// elsewhere — those fail the transpile; these only warn.
func Warnings(prog *ir.Program) []string {
	var w []string

	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		for _, c := range p.Calls {
			q := name + "." + c.Name

			if c.Preflight {
				w = append(w, fmt.Sprintf("call %s: `preflight` runs as an ordinary stage — it executes, but does not gate/abort the run early as it would under mrp", q))
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
