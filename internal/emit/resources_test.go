package emit

import (
	"strings"
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

	// The dynamic directives must take the magnitude of the RAW per-task value
	// (Math.abs wraps the nil-coalesced field read, BEFORE any positivity test,
	// so a negative sentinel provisions |x|) and reach the stage default only
	// in the not-positive arm. Assert those parts rather than the full
	// generated Groovy: a cosmetic template change stays green while an
	// abs-after-fallback or dropped-default regression still fails.
	mem := dynMem("res", 3)
	for _, part := range []string{"memory {", "Math.abs((res?.mem_gb ?: 0)", ": 3;", "task.attempt"} {
		if !strings.Contains(mem, part) {
			t.Errorf("dynMem = %q, missing %q", mem, part)
		}
	}

	cpus := dynCpus("res", 3)
	for _, part := range []string{"cpus {", "Math.abs((res?.threads ?: 0)", ": 3 }"} {
		if !strings.Contains(cpus, part) {
			t.Errorf("dynCpus = %q, missing %q", cpus, part)
		}
	}
}
