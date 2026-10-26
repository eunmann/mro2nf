package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestFile writes body to path, creating its dir, and returns path.
func writeTestFile(t *testing.T, path, body string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}
