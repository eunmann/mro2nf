package emit

import (
	"slices"

	"github.com/eunmann/mro2nf/internal/ir"
)

// The inventory below is the naming contract between this package and
// internal/overrides: Nextflow full-matches a withName selector regex, so the
// overrides converter must enumerate the exact, finite suffix set for each
// naming family — an open-ended `<stage>.*` would let a stage-level override
// for stage SORT silently retune every process of an unrelated stage
// SORT_READS (#112). TestStageProcessNameInventory cross-checks these lists
// against the names the emitter actually generates, so the two cannot drift.

// PlainStageSuffixes returns every suffix this package appends to a stage's
// callable name for the processes that run the stage's own code: the bare
// non-split main, the keyed non-split _MAP, and the split triad in plain and
// fork-keyed (_K) form.
func PlainStageSuffixes() []string {
	return []string{"", "_MAP", "_SPLIT", "_SPLIT_K", "_MAIN", "_MAIN_K", "_JOIN", "_JOIN_K"}
}

// FusedCallSuffixes returns every suffix on a fused per-call process name,
// STAGE_<n>_<pipeline>__<call> (see fusedName/qualify): the bare fused
// bind+main / native scatter / fused chain, the fused bind+split _SP with its
// _MN/_JN phase include-aliases, and the keyed fused bind+main _K (#99).
func FusedCallSuffixes() []string {
	return []string{"", "_SP", "_MN", "_JN", "_K"}
}

// ScatterCallSuffixes returns every suffix on a fork-named process,
// FORK_<n>_<pipeline>__<call> (see forkName/qualify), that runs the stage's
// own main: the keyed element scatter _KS (#99). The bare FORK_ name and its
// _K variant are forkbind helpers, not stage compute.
func ScatterCallSuffixes() []string {
	return []string{"_KS"}
}

// StageProcessNames returns, sorted, every process name the emitter can give
// a task that runs a stage's own code in prog: per stage the plain family, and
// per pipeline call the fused family and the keyed element scatter. Every
// other process the emitter generates is data-plane plumbing.
func StageProcessNames(prog *ir.Program) []string {
	var names []string

	for stage := range prog.Stages {
		for _, s := range PlainStageSuffixes() {
			names = append(names, stage+s)
		}
	}

	for _, p := range prog.Pipelines {
		for _, c := range p.Calls {
			q := qualify(p.Name, c.Name)

			for _, s := range FusedCallSuffixes() {
				names = append(names, "STAGE_"+q+s)
			}

			for _, s := range ScatterCallSuffixes() {
				names = append(names, "FORK_"+q+s)
			}
		}
	}

	slices.Sort(names)

	return names
}
