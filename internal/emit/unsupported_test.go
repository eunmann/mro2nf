package emit_test

import (
	"errors"
	"testing"

	"github.com/eunmann/martian-nextflow/internal/apperror"
	"github.com/eunmann/martian-nextflow/internal/emit"
	"github.com/eunmann/martian-nextflow/internal/frontend"
)

// TestEmitUnsupported asserts that feature combinations the emitter cannot yet
// lower correctly are rejected with a clear UnsupportedError at transpile time,
// rather than producing silently-wrong Nextflow.
func TestEmitUnsupported(t *testing.T) {
	cases := []struct {
		name string
		mro  string
	}{
		{"map call over a sub-pipeline with disabled body", "../../testdata/unsupported/map_over_pipeline.mro"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := frontend.Parse(tc.mro, []string{"../../testdata/unsupported"}, false)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			prog, err := frontend.Lower(ast)
			if err != nil {
				t.Fatalf("lower: %v", err)
			}

			err = emit.Emit(prog, emit.Options{OutDir: t.TempDir(), Mre: "mre"})
			if !errors.Is(err, apperror.ErrUnsupported) {
				t.Errorf("Emit error = %v, want ErrUnsupported", err)
			}
		})
	}
}
