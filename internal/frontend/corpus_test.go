package frontend_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/frontend"
)

// TestCorpusParsesAndLowers is a robustness sweep over rich, real Martian MRO
// files (structs, typed maps, map calls over arrays and maps, projection,
// wildcards, duck typing, disabled). Every file must parse (it is valid MRO
// that `mro check` accepts); lowering must then either succeed or fail with a
// clean apperror — never panic and never return an opaque error.
func TestCorpusParsesAndLowers(t *testing.T) {
	dir := "../../testdata/corpus"

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".mro" {
			continue
		}

		t.Run(entry.Name(), func(t *testing.T) {
			ast, err := frontend.Parse(filepath.Join(dir, entry.Name()), []string{dir}, false)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			// Lower must not panic; a failure must be a typed apperror so the
			// CLI can report it cleanly rather than crash.
			if _, err := frontend.Lower(ast, nil); err != nil {
				if !errors.Is(err, apperror.ErrUnsupported) && !errors.Is(err, apperror.ErrParse) {
					t.Errorf("lower returned a non-apperror error: %v", err)
				} else {
					t.Logf("lower: gracefully unsupported: %v", err)
				}
			}
		})
	}
}
