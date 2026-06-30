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

	// The dynamic directives must use the magnitude of a negative per-task value.
	if d := dynMem("res", 1); !strings.Contains(d, "Math.abs") {
		t.Errorf("dynMem must take the magnitude of a negative per-task mem_gb: %s", d)
	}
	if d := dynCpus("res", 1); !strings.Contains(d, "Math.abs") {
		t.Errorf("dynCpus must take the magnitude of a negative per-task threads: %s", d)
	}
}
