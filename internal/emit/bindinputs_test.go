package emit

import (
	"strings"
	"testing"
)

// TestBindInputsStagesPipeargsDistinctly guards bug 1: a bind process consumes
// its enclosing pipeline's args (pipeargs) and writes its resolved bundle to a
// dir named `args`. A sub-pipeline's pipeargs IS an upstream bind output — a dir
// named `args` — so staging `path pipeargs` (unquoted) lands it under basename
// `args`, the very name the script writes with `-o args`, clobbering the input.
// Pinning the stage name to `pipeargs` keeps producer and consumer distinct.
func TestBindInputsStagesPipeargsDistinctly(t *testing.T) {
	t.Run("non-keyed", func(t *testing.T) {
		block, _ := bindInputs(nil)
		if !strings.Contains(block, "path pipeargs, stageAs: 'pipeargs'") {
			t.Errorf("pipeargs must be staged under a fixed name distinct from the `args` output; got:\n%s", block)
		}
	})

	t.Run("keyed", func(t *testing.T) {
		block, _ := keyedInputs(nil)
		if !strings.Contains(block, "path(pipeargs, stageAs: 'pipeargs')") {
			t.Errorf("keyed pipeargs must be staged under a fixed name distinct from the `args` output; got:\n%s", block)
		}
	})
}
