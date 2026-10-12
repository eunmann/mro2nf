//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeKeysOnTheBuild (#449): a -resume re-run of an unchanged project
// is fully cached, but once the project is re-transpiled by another mro2nf
// build, no task is: Nextflow keys a task on its script text and inputs, not
// on the mre binary, so a task cached by an older build would otherwise hand
// on outputs shaped by that build's mre (fork element dirs without a record
// the new mre args reads). The fixtures cover a map call's forks and the
// split triad.
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

// TestResumeKeysOnlyOnATasksOwnTypes (#286): after one stage's signature
// changes, a -resume re-run of the re-transpiled project re-runs that stage
// but keeps the unrelated stage cached. Every task used to stage the one
// shared types.json, so any type edit re-ran the whole pipeline.
func TestResumeKeysOnlyOnATasksOwnTypes(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	fixture := t.TempDir()
	if err := os.CopyFS(fixture, os.DirFS(filepath.Join(root, "testdata", "diamond_min"))); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	proj := transpileDir(t, fixture)

	if err := runNextflow(t, proj); err != nil {
		t.Fatalf("first run: %v", err)
	}

	mro := filepath.Join(fixture, "pipeline.mro")
	editMRO(t, mro,
		"    in  float b,\n    out float c,", "    in  float b,\n    in  int   unused,\n    out float c,",
		"        b = GEN.y,\n", "        b = GEN.y,\n        unused = 1,\n")

	if err := transpileInto(repoTools(), fixture, "pipeline.mro", proj); err != nil {
		t.Fatalf("re-transpile after the ADD edit: %v", err)
	}

	if err := runNextflow(t, proj, "-resume", "-with-trace", "edited.txt"); err != nil {
		t.Fatalf("-resume after the ADD edit: %v", err)
	}

	status := map[string]string{}
	for _, row := range readTrace(t, filepath.Join(proj, "edited.txt")) {
		status[row.Name] = row.Status
	}

	for name, want := range map[string]bool{"N0_GEN (D.GEN)": true, "N1_ADD (D.ADD)": false} {
		got, ok := status[name]
		if !ok {
			t.Errorf("task %s missing from the trace %v", name, status)

			continue
		}

		if (got == "CACHED") != want {
			t.Errorf("task %s: status %s, want cached=%v", name, got, want)
		}
	}
}

// editMRO applies each old->new replacement pair to the .mro at path,
// failing when a pair's old text is absent.
func editMRO(t *testing.T, path string, pairs ...string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	src := string(data)

	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(src, pairs[i]) {
			t.Fatalf("%s has no %q to edit", path, pairs[i])
		}

		src = strings.Replace(src, pairs[i], pairs[i+1], 1)
	}

	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
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
