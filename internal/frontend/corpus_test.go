package frontend_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorpusParsesAndLowers is a robustness sweep over rich, real Martian MRO
// files (structs, typed maps, map calls over arrays and maps, projection,
// wildcards, duck typing, disabled). Every file must compile (it is valid MRO
// that `mro check` accepts) and lower without a panic.
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
			prog := lower(t, filepath.Join(dir, entry.Name()), []string{dir})
			if len(prog.Stages)+len(prog.Pipelines)+len(prog.Structs) == 0 {
				t.Errorf("%s lowered no callables or structs", entry.Name())
			}
		})
	}
}
