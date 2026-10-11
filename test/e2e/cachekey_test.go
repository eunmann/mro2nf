//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestResumeKeysOnTheBuild (#449): a -resume re-run of an unchanged project
// is fully cached, but once the project is re-transpiled by another mro2nf
// build, no task is: Nextflow keys a task on its script text and inputs, not
// on the mre binary, so a task cached by an older build would otherwise hand
// on outputs shaped by that build's mre (a FORK sidecar without the field the
// new merge reads). The fixtures cover the FORK/MERGE fan-out and the split
// triad. A -native re-transpile is left out: it re-bakes entry_resolved/ with
// a fresh mtime, which re-runs every task whatever the build.
func TestResumeKeysOnTheBuild(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")
	buildBinaries(t)

	other := toolchain{mro2nf: buildStampedMro2nf(t, "e2e-another-build"), mre: repoTools().mre}

	for _, fixture := range []string{"map_fork", "split_test"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, fixture)

			if err := runNextflow(t, proj); err != nil {
				t.Fatalf("first run: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "same.txt"); err != nil {
				t.Fatalf("-resume rerun: %v", err)
			}

			assertAllCached(t, readTrace(t, filepath.Join(proj, "same.txt")))

			if err := transpileInto(other, filepath.Join(root, "testdata", fixture), "pipeline.mro", proj); err != nil {
				t.Fatalf("re-transpile with another build: %v", err)
			}

			if err := runNextflow(t, proj, "-resume", "-with-trace", "other.txt"); err != nil {
				t.Fatalf("-resume after re-transpile: %v", err)
			}

			assertNoneCached(t, readTrace(t, filepath.Join(proj, "other.txt")))
		})
	}
}

// assertNoneCached fails on an empty trace or any CACHED row: a reused task
// has a cache key that misses what changed.
func assertNoneCached(t *testing.T, rows []traceRow) {
	t.Helper()

	if len(rows) == 0 {
		t.Fatal("empty trace, want >0 rows")
	}

	for _, row := range rows {
		if row.Status == "CACHED" {
			t.Errorf("task %s was reused from the cache, want it re-run", row.Name)
		}
	}
}

// buildStampedMro2nf builds mro2nf as `make build` does, stamped with
// version, into a temp dir and returns its path.
func buildStampedMro2nf(t *testing.T, version string) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "mro2nf")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+version, "-o", bin, "./cmd/mro2nf")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stamped mro2nf: %v\n%s", err, out)
	}

	return bin
}
