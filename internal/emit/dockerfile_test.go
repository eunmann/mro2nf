package emit

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyTreeMissingSource pins copyTree's hard-error contract directly:
// callers pre-verify sources (checkContainerSources), so a nonexistent src
// must return an error naming the path — never a silent no-op.
func TestCopyTreeMissingSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "does-not-exist")

	err := copyTree(src, filepath.Join(t.TempDir(), "dst"))
	if err == nil || !strings.Contains(err.Error(), src) {
		t.Errorf("copyTree(missing src): want error naming %q, got %v", src, err)
	}
}
