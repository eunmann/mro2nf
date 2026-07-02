//go:build e2e

package e2e

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// spikeExpectedOK is every OK[...] assertion the #13 de-bundle spike must
// emit (8 across its four shapes).
const spikeExpectedOK = 8

// TestSpikeDebundle validates the #13 de-bundle spike on both staging models:
// a symlink work dir (local/HealthOmics) and a copy work dir (the S3 proxy).
// It also asserts the zero-copy property — no bundle f/ dir anywhere in the
// work tree (each producer materializes every leaf exactly once). Port of
// spike_debundle.sh; the shell version's `find | grep -q` SIGPIPE hazard does
// not exist here.
func TestSpikeDebundle(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	modes := []struct {
		name  string
		cloud bool
	}{
		{"local", false},
		{"s3proxy", true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			run := t.TempDir()
			copySpike(t, filepath.Join(root, "spike", "13-debundle"), run)

			args := []string{"run", "main.nf"}

			if mode.cloud {
				cfg := "process { scratch = true; stageInMode = 'copy'; stageOutMode = 'copy' }\n"
				if err := os.WriteFile(filepath.Join(run, "cloud.config"), []byte(cfg), 0o644); err != nil {
					t.Fatal(err)
				}

				args = append(args, "-c", "cloud.config")
			}

			cmd := exec.Command("nextflow", args...)
			cmd.Dir = run

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("nextflow: %v\n%s", err, tail(out, 12))
			}

			text := string(out)
			if strings.Contains(text, "FAIL[") {
				t.Fatalf("a shape assertion failed:\n%s", tail(out, 20))
			}

			if n := strings.Count(text, "OK["); n != spikeExpectedOK {
				t.Fatalf("got %d/%d OK assertions:\n%s", n, spikeExpectedOK, tail(out, 20))
			}

			// Zero-copy: no bundle f/ dir anywhere in the work tree.
			err = filepath.WalkDir(filepath.Join(run, "work"), func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil // a task dir vanishing mid-walk is fine
				}

				if d.IsDir() && d.Name() == "f" {
					t.Errorf("found a bundle f/ dir (expected zero byte-copy): %s", path)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("walk work dir: %v", err)
			}
		})
	}
}

// copySpike copies the spike project into dst, keeping bin/ executable.
func copySpike(t *testing.T, src, dst string) {
	t.Helper()

	if out, err := exec.Command("cp", "-r", src+"/.", dst).CombinedOutput(); err != nil {
		t.Fatalf("copy spike: %v\n%s", err, out)
	}

	bins, err := filepath.Glob(filepath.Join(dst, "bin", "*.py"))
	if err != nil {
		t.Fatal(err)
	}

	for _, b := range bins {
		if err := os.Chmod(b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
