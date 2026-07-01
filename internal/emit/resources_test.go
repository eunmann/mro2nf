package emit

import (
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// TestResourceDirectivesAbsNegativeSentinel guards the adaptive-resource model:
// a negative request is Martian's "at least |x|" sentinel, which its cluster path
// resolves to |x|. The emitted Nextflow directives must provision |x|, not floor
// a negative to 1 (static) or fall back to the stage default (dynamic).
func TestResourceDirectivesAbsNegativeSentinel(t *testing.T) {
	s := &ir.Stage{Resources: ir.Resources{Threads: -2, MemGB: -4}}

	if got := cpusOf(s); got != 2 {
		t.Errorf("cpusOf(threads=-2) = %d, want 2", got)
	}
	if got := memOf(s); got != 4 {
		t.Errorf("memOf(mem_gb=-4) = %d, want 4", got)
	}

	// The dynamic directives take the magnitude of the per-task value at the right
	// place: abs the raw val, fall back to the stage default only when it's zero.
	wantMem := "memory { def m = Math.abs((res?.mem_gb ?: 0) as double); m = m > 0 ? m : 1; (m * task.attempt) + ' GB' }"
	if d := dynMem("res", 1); d != wantMem {
		t.Errorf("dynMem =\n  %s\nwant\n  %s", d, wantMem)
	}

	wantCpus := "cpus { def t = Math.abs((res?.threads ?: 0) as double); t > 0 ? Math.max(1, Math.ceil(t) as int) : 1 }"
	if d := dynCpus("res", 1); d != wantCpus {
		t.Errorf("dynCpus =\n  %s\nwant\n  %s", d, wantCpus)
	}
}
