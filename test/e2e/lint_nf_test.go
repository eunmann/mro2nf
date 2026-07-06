//go:build e2e

package e2e

// Static Nextflow lint gate (#48): transpile every fixture and run
// `nextflow lint` over the generated project. `nextflow lint` (>= 25.04) drives
// the formal language-server parser, so a Groovy syntax error in a rarely-run
// emission branch — a keyed variant, a disable gate, a fused split — becomes an
// immediate, precise CI failure with file/line, independent of containers, AWS,
// or which paths the golden fixtures happen to execute. It fails only on ERRORS
// (a real syntax bug); style warnings on the generated code exit 0.
//
// The matrix is fixtures × lintConfigs (~700 combos), but a per-combo
// `nextflow lint` pays a ~2s JVM boot that dwarfs the linting itself (699s on
// the 2-core CI runner). Two multiplicative fixes keep the gate fast without
// losing a single verdict:
//
//  1. Dedupe: most flag configs are no-ops for most fixtures, so the combos
//     collapse to ~half as many byte-identical emissions. Every combo still
//     transpiles (milliseconds) and hashes its tree; byte-identical projects
//     necessarily lint identically, so each unique emission is linted once
//     and the verdict fans back out to every member combo by name.
//  2. Batching: one `nextflow lint` invocation lints many project dirs with
//     per-file JSON diagnostics, so the unique emissions share a handful of
//     JVMs instead of one each. Every emission ships the identical static
//     lib/ (asserted below), so a single -project-dir serves the batch.
//
// An injected-syntax-error guard project rides every batch and MUST fail,
// proving end-to-end that the gate still has teeth.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// lintConfigs is the emission-branch dimension (#122): each opt-in flag routes
// the emitter down generator branches the default emission never reaches
// (native scatter/merge, the direct-call runner scripts, fused-chain
// processes, folded-off null channels), so each fixture is linted under every
// single flag plus the compositions that route down distinct combined
// branches: -native -native-runner, and -native with each plan lever
// (TestNativeCombos proves those emit Groovy neither flag emits alone).
var lintConfigs = []struct {
	name  string
	flags []string
}{
	{"default", nil},
	{"native", []string{"-native"}},
	{"native-runner", []string{"-native-runner"}},
	{"fuse-chains", []string{"-fuse-chains"}},
	{"fold-disables", []string{"-fold-disables"}},
	{"native+native-runner", []string{"-native", "-native-runner"}},
	{"native+fuse-chains", []string{"-native", "-fuse-chains"}},
	{"native+fold-disables", []string{"-native", "-fold-disables"}},
}

// minNextflowMajor/Minor is the first release with `nextflow lint`.
const (
	minNextflowMajor = 25
	minNextflowMinor = 4
)

// lintBatchSize bounds how many unique projects one `nextflow lint`
// invocation covers, keeping the argument list and the JVM's parse workload
// bounded as the fixture matrix grows. One JVM handled the full ~370-project
// set comfortably, so this is headroom, not tuning.
const lintBatchSize = 150

// TestNextflowLint lints the generated project for every testdata fixture
// under every lintConfigs flag set. It enumerates the fixtures (rather than a
// hand-kept list) so any new fixture is linted automatically and the branch
// space is covered by construction. ANY transpile failure is a test failure:
// no flag/fixture combination is refused today, so there is nothing to skip.
// If a future flag introduces a genuine SevError refusal, add an explicit
// expected-refusal allowlist entry for that exact combo THEN — loud by
// default, never classified by error-message substring.
func TestNextflowLint(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")
	requireNextflowLint(t)
	buildBinaries(t)

	parent := t.TempDir()
	combos := transpileLintCombos(t, parent)
	repByGroup, reps := dedupeLintCombos(combos)

	var errsByProj map[string][]string

	if len(reps) > 0 { // every combo failing to transpile still reports below
		t.Logf("linting %d unique emissions for %d fixture/config combos (%.1fx dedupe)",
			len(reps), len(combos), float64(len(combos))/float64(len(reps)))

		assertUniformLib(t, parent, reps)

		guard := injectLintGuard(t, parent, reps[0])
		errsByProj = batchLint(t, parent, append(reps, guard))

		// The gate must have teeth: if a deliberately broken emission lints
		// clean, a real broken emission couldn't surface either and the whole
		// suite would be a silent green skip.
		if len(errsByProj[guard]) == 0 {
			t.Fatalf("lint gate has no teeth: injected syntax error in %s produced no lint errors", guard)
		}
	}

	for _, c := range combos {
		t.Run(c.fixture+"/"+c.config, func(t *testing.T) {
			t.Parallel() // verdicts are precomputed; subtests only read

			if c.transpileErr != nil {
				t.Fatalf("transpile %s under %v: %v", c.fixture, c.flags, c.transpileErr)
			}

			if errs := errsByProj[repByGroup[c.group]]; len(errs) > 0 {
				t.Errorf("nextflow lint reported errors for %s under %v (linted as %s):\n%s",
					c.fixture, c.flags, repByGroup[c.group], strings.Join(errs, "\n"))
			}
		})
	}
}

// lintCombo is one (fixture, flag config) cell of the lint matrix, transpiled
// into <parent>/<dir>.
type lintCombo struct {
	fixture, config string
	flags           []string
	dir             string // project dir name under parent; the attribution key
	transpileErr    error
	group           string // emission content digest; empty on transpile failure
}

// transpileLintCombos transpiles every fixture under every lintConfigs flag
// set into a named directory under parent and digests each emission.
// Transpile failures are recorded, not fatal, so one broken combo doesn't
// hide the verdicts for the rest.
func transpileLintCombos(t *testing.T, parent string) []*lintCombo {
	t.Helper()

	fixtures := lintFixtures(t)
	combos := make([]*lintCombo, 0, len(fixtures)*len(lintConfigs))

	for _, fx := range fixtures {
		for _, cfg := range lintConfigs {
			c := &lintCombo{
				fixture: fx, config: cfg.name, flags: cfg.flags,
				dir: fx + "__" + cfg.name,
			}
			combos = append(combos, c)

			proj := filepath.Join(parent, c.dir)
			if err := transpileInto(filepath.Join(root, "testdata", fx), proj, cfg.flags...); err != nil {
				c.transpileErr = err

				continue
			}

			c.group = treeDigest(t, proj)
		}
	}

	return combos
}

// dedupeLintCombos groups combos by emission digest and picks the first
// member's directory as the group's lint representative: byte-identical
// projects necessarily get identical lint verdicts, so each unique emission
// is linted exactly once and the verdict fans back out to every member. It
// returns the representative dir per digest plus the representatives in
// first-seen (deterministic) order.
func dedupeLintCombos(combos []*lintCombo) (map[string]string, []string) {
	repByGroup := map[string]string{}

	var reps []string

	for _, c := range combos {
		if c.transpileErr != nil {
			continue
		}

		if _, ok := repByGroup[c.group]; !ok {
			repByGroup[c.group] = c.dir
			reps = append(reps, c.dir)
		}
	}

	return repByGroup, reps
}

// treeDigest collapses hashTree into one content digest for the whole project
// dir, so byte-identical emissions map to the same key regardless of which
// directory they were emitted into.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()

	tree := hashTree(t, dir)

	var manifest strings.Builder
	for _, rel := range slices.Sorted(maps.Keys(tree)) {
		manifest.WriteString(rel)
		manifest.WriteByte(0)
		manifest.WriteString(tree[rel])
		manifest.WriteByte('\n')
	}

	sum := sha256.Sum256([]byte(manifest.String()))

	return hex.EncodeToString(sum[:])
}

// assertUniformLib proves every unique emission ships a byte-identical lib/
// tree. Batch linting resolves the driver-classpath classes (lib/*.groovy)
// from a single -project-dir, so if a flag ever made lib/ config-dependent,
// batching would silently lint some projects against the wrong helper
// classes — fail loudly instead of degrading coverage.
func assertUniformLib(t *testing.T, parent string, reps []string) {
	t.Helper()

	want := hashTree(t, filepath.Join(parent, reps[0], "lib"))

	for _, rep := range reps[1:] {
		got := hashTree(t, filepath.Join(parent, rep, "lib"))
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("lib/ differs between %s and %s; batch lint assumes one shared -project-dir classpath (-%s +%s):\n%s",
				reps[0], rep, reps[0], rep, diff)
		}
	}
}

// injectLintGuard copies a clean emission and appends a Groovy syntax error
// to its main.nf. The batch MUST report an error for it, proving on every run
// that the whole lint path — batch invocation, JSON parsing, per-project
// attribution — can actually surface a broken emission. The `__`-free name
// cannot collide with a <fixture>__<config> project dir.
func injectLintGuard(t *testing.T, parent, rep string) string {
	t.Helper()

	const guard = "zz-lint-guard"
	if err := os.CopyFS(filepath.Join(parent, guard), os.DirFS(filepath.Join(parent, rep))); err != nil {
		t.Fatalf("copy %s -> %s: %v", rep, guard, err)
	}

	f, err := os.OpenFile(filepath.Join(parent, guard, "main.nf"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open guard main.nf: %v", err)
	}

	if _, err := f.WriteString("\nworkflow { if ( }\n"); err != nil {
		t.Fatalf("inject syntax error: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close guard main.nf: %v", err)
	}

	return guard
}

// lintDiag is one diagnostic from `nextflow lint -output json`.
type lintDiag struct {
	Filename    string `json:"filename"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	Message     string `json:"message"`
}

// batchLint lints the given project directories (relative to parent) in a few
// `nextflow lint` invocations instead of one JVM per project — ~95% of a
// single-project lint is JVM boot. Every project ships the identical lib/
// (assertUniformLib), so the chunk's first project serves as -project-dir for
// classpath resolution, and the JSON diagnostics carry project-relative paths
// that attribute each error back to its emission. Returns error strings keyed
// by project dir name; projects absent from the map linted clean.
func batchLint(t *testing.T, parent string, projects []string) map[string][]string {
	t.Helper()

	errsByProj := map[string][]string{}

	for chunk := range slices.Chunk(projects, lintBatchSize) {
		args := append([]string{"lint", "-output", "json", "-project-dir", chunk[0]}, chunk...)
		cmd := exec.Command("nextflow", args...)
		cmd.Dir = parent

		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		// A nonzero exit just means diagnostics were found; the JSON on
		// stdout is the verdict (progress lines go to stderr).
		out, runErr := cmd.Output()

		var report struct {
			Errors []lintDiag `json:"errors"`
		}
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatalf("nextflow lint batch produced no JSON (%v; run: %v):\nstdout:\n%s\nstderr:\n%s",
				err, runErr, out, stderr.Bytes())
		}

		if runErr != nil && len(report.Errors) == 0 {
			t.Fatalf("nextflow lint batch failed without diagnostics: %v\nstderr:\n%s", runErr, stderr.Bytes())
		}

		for _, d := range report.Errors {
			proj, rest, ok := strings.Cut(filepath.ToSlash(d.Filename), "/")
			if !ok || !slices.Contains(chunk, proj) {
				t.Fatalf("lint error for unattributable path %q: %s", d.Filename, d.Message)
			}

			errsByProj[proj] = append(errsByProj[proj],
				fmt.Sprintf("%s:%d:%d: %s", rest, d.StartLine, d.StartColumn, d.Message))
		}
	}

	return errsByProj
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
