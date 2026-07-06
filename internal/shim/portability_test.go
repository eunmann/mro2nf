package shim_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
)

// TestScanOutputForScratchRefs checks the embedded-scratch-path guard: a small
// text output that names the task work dir is flagged; a clean one, a binary
// one, and a match outside the bundle's f/ dir are not.
func TestScanOutputForScratchRefs(t *testing.T) {
	bundle := t.TempDir()
	scratch := "/tmp/nxf.ABC123/ab/cd0011"
	fdir := filepath.Join(bundle, "f")
	if err := os.MkdirAll(fdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A JSON manifest that bakes the scratch path into content — the smell.
	write(t, filepath.Join(fdir, "L0000.json"),
		`{"bam": "`+scratch+`/main/files/out.bam", "n": 3}`)
	// A clean output — no scratch path.
	write(t, filepath.Join(fdir, "L0001.txt"), "totally fine, no paths here")
	// A binary output that happens to contain the bytes — must be skipped.
	write(t, filepath.Join(fdir, "L0002.bin"), "head\x00"+scratch+"/x")
	// A match OUTSIDE f/ (e.g. the payload) must not be walked.
	write(t, filepath.Join(bundle, "data.json"), scratch+"/whatever")

	got := shim.ScanOutputForScratchRefs(bundle, scratch)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}

	if got[0].File != filepath.Join("f", "L0000.json") {
		t.Errorf("finding file = %q, want f/L0000.json", got[0].File)
	}

	want := scratch + "/main/files/out.bam"
	if got[0].Path != want {
		t.Errorf("finding path = %q, want %q", got[0].Path, want)
	}

	// An empty prefix is a no-op (never scans), and a clean scratch prefix that
	// nothing references yields nothing.
	if refs := shim.ScanOutputForScratchRefs(bundle, ""); refs != nil {
		t.Errorf("empty prefix must yield no findings, got %+v", refs)
	}

	if refs := shim.ScanOutputForScratchRefs(bundle, "/no/such/prefix/here"); refs != nil {
		t.Errorf("unreferenced prefix must yield no findings, got %+v", refs)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
