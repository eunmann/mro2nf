package main

import (
	"path/filepath"
	"testing"
)

// TestResolveStageCode checks that a stage's src path is resolved to an absolute
// path: a bare comp binary name (e.g. "cr_lib") is joined to the MRO dir (a
// resolvable file/symlink is expected there — a compiled adapter needs a real
// path for current_exe, not a bare argv[0]), as is a relative py path; an
// absolute path is passed through.
func TestResolveStageCode(t *testing.T) {
	mroDir := "/opt/cellranger/mro"

	compAbs, err := filepath.Abs(filepath.Join(mroDir, "cr_lib"))
	if err != nil {
		t.Fatal(err)
	}

	relAbs, err := filepath.Abs(filepath.Join(mroDir, "stages/reads/foo"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, src, want string
	}{
		{"bare comp binary", "cr_lib", compAbs},
		{"relative py path", "stages/reads/foo", relAbs},
		{"absolute path", "/opt/cellranger/bin/tool", "/opt/cellranger/bin/tool"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveStageCode(c.src, mroDir); got != c.want {
				t.Errorf("resolveStageCode(%q) = %q, want %q", c.src, got, c.want)
			}
		})
	}
}
