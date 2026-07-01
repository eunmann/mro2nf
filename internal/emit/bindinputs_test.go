package emit

import (
	"strings"
	"testing"
)

// TestBindInputsStagesPipeargsDistinctly guards bug 1: a bind process consumes
// its enclosing pipeline's args (pipeargs) and writes its resolved bundle to a
// dir named `args`. The de-bundled pipeargs is reconstructed under its own dir
// `pipeargs/` (data.json + f/ leaves), distinct from the `-o args` output dir, so
// producer and consumer never clobber.
func TestBindInputsStagesPipeargsDistinctly(t *testing.T) {
	t.Run("non-keyed", func(t *testing.T) {
		block, _ := bindInputs(nil)
		if !strings.Contains(block, "tuple(path('pipeargs/data.json'), path('pipeargs/f/*'))") {
			t.Errorf("pipeargs must be reconstructed under a fixed dir distinct from the `args` output; got:\n%s", block)
		}
	})

	t.Run("keyed", func(t *testing.T) {
		block, _ := keyedInputs(nil)
		if !strings.Contains(block, "path(pipeargs, stageAs: 'pipeargs')") {
			t.Errorf("keyed pipeargs must be staged under a fixed name distinct from the `args` output; got:\n%s", block)
		}
	})
}
