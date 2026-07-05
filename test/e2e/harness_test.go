//go:build e2e

// Package e2e drives the transpiled pipelines end-to-end under real Nextflow.
// It is the Go replacement for the test/e2e/*.sh harnesses (ported script by
// script; the live AWS runbooks stay shell). Run via:
//
//	make test-e2e-go   # go test -tags e2e -count=1 ./test/e2e/
//
// -count=1 is load-bearing: Go's test cache would otherwise return a cached
// "ok" without running Nextflow — the Go-native version of a silent skip.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// root is the repo root, resolved once in TestMain.
var root string

// TestMain builds the transpiler + shim once and pins the Nextflow environment
// the shell harnesses used (no ANSI redraw, no version check, small JVM).
func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "getwd:", err)
		os.Exit(1)
	}

	root = filepath.Dir(filepath.Dir(wd)) // test/e2e -> repo root

	os.Setenv("NXF_ANSI_LOG", "false")
	os.Setenv("NXF_DISABLE_CHECK_LATEST", "true")

	if os.Getenv("NXF_OPTS") == "" {
		os.Setenv("NXF_OPTS", "-Xms256m -Xmx1g -XX:+UseSerialGC")
	}

	os.Exit(m.Run())
}

// requireTools skips the test when a prerequisite is missing — unless
// E2E_REQUIRE=1 (set in CI), where a missing tool must FAIL, not silently
// green-skip the gate.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()

	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err == nil {
			continue
		}

		if os.Getenv("E2E_REQUIRE") == "1" {
			t.Fatalf("%s not found (required by E2E_REQUIRE=1)", tool)
		}

		t.Skipf("%s not found", tool)
	}
}

var buildOnce sync.Once

// buildBinaries builds ./mro2nf and ./mre at the repo root exactly once per
// test process.
func buildBinaries(t *testing.T) {
	t.Helper()

	buildOnce.Do(func() {
		for _, bin := range []string{"mro2nf", "mre"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(root, bin), "./cmd/"+bin)
			cmd.Dir = root

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build %s: %v\n%s", bin, err, out)
			}
		}
	})
}

// transpile runs mro2nf on a testdata fixture into a fresh temp project dir,
// adding -mrjob automatically when the fixture ships one. extra flags are
// appended before the .mro argument.
func transpile(t *testing.T, fixture string, extra ...string) string {
	t.Helper()

	return transpileDir(t, filepath.Join(root, "testdata", fixture), extra...)
}

// transpileDir is transpile for a fixture at an arbitrary directory (e.g. the
// bench/ pipelines).
func transpileDir(t *testing.T, dir string, extra ...string) string {
	t.Helper()

	proj, out, err := transpileDirErr(t, dir, extra...)
	if err != nil {
		t.Fatalf("transpile %s: %v\n%s", dir, err, out)
	}

	return proj
}

// transpileDirErr is transpileDir but returns the transpiler's combined output
// and error instead of failing the test, so a caller sweeping flag/fixture
// combinations can treat an expected refusal (a SevError flag/pipeline
// conflict) explicitly rather than as a harness fatal.
func transpileDirErr(t *testing.T, dir string, extra ...string) (string, []byte, error) {
	t.Helper()
	buildBinaries(t)

	proj := t.TempDir()

	args := []string{
		"-o", proj,
		"-mre", filepath.Join(root, "mre"),
		"-shell", filepath.Join(root, "vendor-martian", "python", "martian_shell.py"),
		"-mropath", dir,
	}

	if mrjob := filepath.Join(dir, "mrjob.sh"); fileExists(mrjob) {
		args = append(args, "-mrjob", mrjob)
	}

	args = append(args, extra...)
	args = append(args, filepath.Join(dir, "pipeline.mro"))

	cmd := exec.Command(filepath.Join(root, "mro2nf"), args...)
	cmd.Dir = root

	out, err := cmd.CombinedOutput()

	return proj, out, err
}

// runNextflow runs `nextflow run main.nf <args...>` in proj, always capturing
// the combined output to proj/run.log. On error the log tail is attached to
// the returned error so failures are never silent.
func runNextflow(t *testing.T, proj string, args ...string) error {
	t.Helper()

	cmd := exec.Command("nextflow", append([]string{"run", "main.nf"}, args...)...)
	cmd.Dir = proj
	// Every run pays a fresh JVM, so startup dominates suite time: skip the
	// remote version/plugin check (no fixture declares plugins), drop ANSI
	// rendering, and cap JIT tiering + heap — the pipelines are trivial
	// compute, and the smaller heap lets more runs overlap under -parallel.
	// An inherited NXF_OPTS (proxy, truststore) is PREPENDED so it survives:
	// within one NXF_OPTS the later flags win, keeping the heap cap. An
	// explicitly exported NXF_OFFLINE is honored (fresh-launcher bootstrap
	// needs the network once).
	env := append(os.Environ(),
		"NXF_ANSI_LOG=false",
		"NXF_OPTS="+strings.TrimSpace(os.Getenv("NXF_OPTS")+" -XX:TieredStopAtLevel=1 -Xms64m -Xmx512m"),
	)
	if os.Getenv("NXF_OFFLINE") == "" {
		env = append(env, "NXF_OFFLINE=true")
	}

	cmd.Env = env

	out, err := cmd.CombinedOutput()

	_ = os.WriteFile(filepath.Join(proj, "run.log"), out, 0o644)

	if err != nil {
		return fmt.Errorf("nextflow run: %w\n%s", err, tail(out, 8))
	}

	return nil
}

// tail returns the last n lines of b, indented for log attachment.
func tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return "    " + strings.Join(lines, "\n    ")
}

// goldenJSON asserts the JSON document at gotPath structurally equals the one
// at goldenPath.
func goldenJSON(t *testing.T, gotPath, goldenPath string) {
	t.Helper()

	var got, want any

	readJSON(t, gotPath, &got)
	readJSON(t, goldenPath, &want)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("pipeline outs mismatch (-golden +got):\n%s", diff)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// traceRow is one task attempt from a `-with-trace` TSV file.
type traceRow struct {
	Name   string
	Status string
	Exit   int
}

// readTrace parses a Nextflow trace file into typed rows (replacing the awk
// column indexing that already caused one harness bug).
func readTrace(t *testing.T, path string) []traceRow {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace %s: %v", path, err)
	}

	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) == 0 {
		return nil
	}

	cols := map[string]int{}
	for i, h := range strings.Split(string(lines[0]), "\t") {
		cols[h] = i
	}

	rows := make([]traceRow, 0, len(lines)-1)

	for _, line := range lines[1:] {
		f := strings.Split(string(line), "\t")

		row := traceRow{Name: f[cols["name"]], Status: f[cols["status"]], Exit: -1}
		if n, err := strconv.Atoi(f[cols["exit"]]); err == nil {
			row.Exit = n
		}

		rows = append(rows, row)
	}

	return rows
}

// attempts counts trace rows whose task name contains name.
func attempts(rows []traceRow, name string) []traceRow {
	var out []traceRow

	for _, r := range rows {
		if strings.Contains(r.Name, name) {
			out = append(out, r)
		}
	}

	return out
}
