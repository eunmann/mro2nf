package gen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestValidateRecoversCompilerPanic feeds the compiler the shrink candidate
// of seed 192 it panicked on (testdata/compiler_panic.mro, which projects a
// pipeline output r1 that P1 lacks), then shrinks seed 192's program a step,
// which reaches that candidate: both must report the panic, not crash.
func TestValidateRecoversCompilerPanic(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "compiler_panic.mro"))
	if err != nil {
		t.Fatal(err)
	}

	if err := compileMRO(string(src)); !errors.Is(err, errCompilerPanic) {
		t.Errorf("compileMRO err = %v, want errCompilerPanic", err)
	}

	p, err := Generate(192)
	if err != nil {
		t.Fatal(err)
	}

	panicked := false

	for _, r := range reductions(p) {
		c := p.Clone()
		if r(c) {
			c.tidy()
			panicked = panicked || errors.Is(Validate(c), errCompilerPanic)
		}
	}

	if !panicked {
		t.Error("no reduction of seed 192 makes the compiler panic, so this test no longer reaches the recovery")
	}

	if len(candidates(p)) == 0 {
		t.Error("seed 192 has no shrink candidates")
	}
}
