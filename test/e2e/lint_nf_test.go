//go:build e2e

package e2e

// Static Nextflow lint gate (#48): transpile every fixture and run
// `nextflow lint` over the generated project. `nextflow lint` (>= 25.04) drives
// the formal language-server parser, so a Groovy syntax error in a rarely-run
// emission branch — a keyed variant, a disable gate, a fused split — becomes an
// immediate, precise CI failure with file/line, independent of containers, AWS,
// or which paths the golden fixtures happen to execute. It fails only on ERRORS
// (a real syntax bug); style warnings on the generated code exit 0.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// minNextflowMajor/Minor is the first release with `nextflow lint`.
const (
	minNextflowMajor = 25
	minNextflowMinor = 4
)

// TestNextflowLint lints the generated project for every testdata fixture. It
// enumerates the fixtures (rather than a hand-kept list) so any new fixture is
// linted automatically and the branch space is covered by construction.
func TestNextflowLint(t *testing.T) {
	requireTools(t, "nextflow", "java")
	requireNextflowLint(t)

	for _, fx := range lintFixtures(t) {
		t.Run(fx, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, fx)

			cmd := exec.Command("nextflow", "lint", ".")
			cmd.Dir = proj

			out, err := cmd.CombinedOutput()

			if err != nil {
				// nextflow lint prints diagnostics grouped by file in path order
				// with warnings interleaved, so a tail can bury the error's
				// file/line under a later file's warnings. Surface the error
				// lines specifically, then the full output for context.
				t.Fatalf("nextflow lint reported errors for %s:\n%s\n--- full output ---\n%s",
					fx, lintErrorLines(out), out)
			}
		})
	}
}

// lintErrorLines returns the `Error …:<line>:<col>:` diagnostic lines from
// `nextflow lint` output (and the ❌ summary), so a failure always names the
// location regardless of where the erroring file sorts among warnings.
func lintErrorLines(out []byte) string {
	var errs []string

	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Error") || strings.Contains(line, "❌") {
			errs = append(errs, trimmed)
		}
	}

	if len(errs) == 0 {
		return "(no Error line found; see full output below)"
	}

	return strings.Join(errs, "\n")
}

// lintFixtures returns every testdata fixture directory that has a pipeline.mro.
func lintFixtures(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "testdata"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	var fixtures []string

	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(root, "testdata", e.Name(), "pipeline.mro")) {
			fixtures = append(fixtures, e.Name())
		}
	}

	if len(fixtures) == 0 {
		t.Fatal("no testdata fixtures found")
	}

	return fixtures
}

// requireNextflowLint asserts the installed Nextflow is new enough to have the
// `lint` subcommand. A too-old install FAILS loudly rather than silently
// skipping, so a stale local Nextflow cannot make the gate a no-op.
func requireNextflowLint(t *testing.T) {
	t.Helper()

	out, err := exec.Command("nextflow", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow -version: %v\n%s", err, out)
	}

	major, minor := parseNextflowVersion(t, string(out))
	if major < minNextflowMajor || (major == minNextflowMajor && minor < minNextflowMinor) {
		t.Fatalf("nextflow %d.%02d is too old for `nextflow lint`; need >= %d.%02d",
			major, minor, minNextflowMajor, minNextflowMinor)
	}
}

// parseNextflowVersion extracts the major and minor version from `nextflow
// -version` output (a line like "      version 26.04.4 build 12445").
func parseNextflowVersion(t *testing.T, out string) (int, int) {
	t.Helper()

	m := regexp.MustCompile(`version\s+(\d+)\.(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse nextflow version from:\n%s", out)
	}

	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2]) // "04" parses to 4

	return major, minor
}
