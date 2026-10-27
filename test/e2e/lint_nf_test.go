//go:build e2e

package e2e

// Static Nextflow lint gate (#48): transpile every fixture and run
// `nextflow lint` over the generated project. `nextflow lint` (>= 26.04) drives
// the formal language-server parser, so a Groovy syntax error in rarely-run
// wiring — a split triad, a gate, a fork's gather — becomes an immediate,
// precise CI failure with file/line, independent of containers, AWS, or which
// paths the golden fixtures happen to execute. It fails on every error and
// every warning: the generated projects lint warning-free (#343).
//
// A per-project `nextflow lint` pays a ~2s JVM boot that dwarfs the linting
// itself. So one `nextflow lint` invocation lints many project dirs with
// per-file JSON diagnostics, and the emissions share a handful of JVMs instead
// of one each. No emission holds anything `nextflow lint` resolves from its
// -project-dir (asserted below), so a single -project-dir serves the batch.
//
// EVERY chunk carries its own injected-syntax-error guard project that MUST
// report exactly its injected error, proving per invocation that the whole
// lint path — invocation, JSON parsing, per-project attribution — still has
// teeth. A single suite-wide guard would ride one chunk and let another
// chunk's silently broken pipeline pass green.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/test/corpus"
	"github.com/eunmann/mro2nf/test/wide"
)

// lintBatchSize bounds how many projects one `nextflow lint` invocation
// covers, keeping the argument list and the JVM's parse workload bounded as
// the fixture matrix grows. One JVM handled the full ~370-project set
// comfortably, so this is headroom, not tuning. Each chunk reserves one of
// these slots for its guard project, so real emissions batch in groups of
// lintBatchSize-1.
const lintBatchSize = 150

// TestNextflowLint lints the generated project for every testdata fixture
// and every corpus invocation Martian accepts (test/corpus). It enumerates
// them (rather than a hand-kept list) so any new fixture is linted
// automatically. ANY transpile failure is a test failure, except for a
// transpileRefused fixture, which must fail to transpile.
func TestNextflowLint(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")
	requireNextflowLint(t)
	buildBinaries(t)

	parent := t.TempDir()
	emissions := transpileLintEmissions(t, parent)
	projects := transpiledProjects(emissions)

	var errsByProj map[string][]string

	if len(projects) > 0 { // every invocation failing to transpile still reports below
		t.Logf("linting %d emissions of %d invocations", len(projects), len(emissions))

		assertPathIndependent(t, emissions)
		assertNoProjectDirContext(t, parent, projects)

		errsByProj = batchLint(t, parent, projects)
	}

	for _, e := range emissions {
		t.Run(e.project, func(t *testing.T) {
			t.Parallel() // verdicts are precomputed; subtests only read

			if reason, refused := transpileRefused[e.project]; refused {
				if e.transpileErr == nil {
					t.Errorf("transpile %s succeeded; it must be refused: %s", e.project, reason)
				}

				return
			}

			if e.transpileErr != nil && strings.HasPrefix(e.project, lintCorpusPrefix) &&
				strings.Contains(e.transpileErr.Error(), keywordInputRefusal) {
				t.Skipf("refused as docs/feature-coverage.md documents (keyword-input): %v", e.transpileErr)
			}

			if e.transpileErr != nil {
				t.Fatalf("transpile %s: %v", e.project, e.transpileErr)
			}

			if errs := errsByProj[e.project]; len(errs) > 0 {
				t.Errorf("nextflow lint reported diagnostics for %s:\n%s", e.project, strings.Join(errs, "\n"))
			}
		})
	}
}

// TestNextflowLintSubworkflows (#541): no testdata fixture's project is
// large enough for the modules below subworkflows/, which a project too
// large for one workflow calls, so TestNextflowLint lints none; this lints
// a generated pipeline's project that has them. A copy with a syntax error
// in one module, linted in the same invocation, must report it, so the
// modules are proven linted, not passed over.
func TestNextflowLintSubworkflows(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")
	requireNextflowLint(t)
	buildBinaries(t)

	src := t.TempDir()
	if err := wide.New(8, 40, 0).Write(src); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()

	const proj, broken = "generated_wide", "generated_wide_broken"
	if err := transpileInto(repoTools(), src, "pipeline.mro", filepath.Join(parent, proj)); err != nil {
		t.Fatal(err)
	}

	modules, err := filepath.Glob(filepath.Join(parent, proj, "subworkflows", "*.nf"))
	if err != nil || len(modules) == 0 {
		t.Fatalf("the generated project has no module below subworkflows/ to lint (%v): grow the pipeline past one workflow", err)
	}

	breakModule(t, filepath.Join(parent, proj), filepath.Join(parent, broken), filepath.Base(modules[0]))

	errs := batchLint(t, parent, []string{proj, broken})
	if len(errs[proj]) > 0 {
		t.Errorf("nextflow lint reported diagnostics for the %d-module project:\n%s", len(modules), strings.Join(errs[proj], "\n"))
	}

	want := "subworkflows/" + filepath.Base(modules[0]) + ":"
	if !slices.ContainsFunc(errs[broken], func(e string) bool { return strings.HasPrefix(e, want) }) {
		t.Errorf("nextflow lint reported %q for a syntax error in %s, want an error there", errs[broken], want)
	}
}

// breakModule copies the project proj to dst and appends a syntax error to
// its sub-workflow module.
func breakModule(t *testing.T, proj, dst, module string) {
	t.Helper()

	if err := os.CopyFS(dst, os.DirFS(proj)); err != nil {
		t.Fatalf("copy %s -> %s: %v", proj, dst, err)
	}

	f, err := os.OpenFile(filepath.Join(dst, "subworkflows", module), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", module, err)
	}

	if _, err := f.WriteString("\nworkflow BROKEN { if ( }\n"); err != nil {
		t.Fatalf("inject syntax error: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", module, err)
	}
}

// lintEmission is one invocation's project, transpiled into
// <parent>/<project>, the dir name the diagnostics attribute to: a
// fixture's name, or corpus_ and a corpus entry's slug.
type lintEmission struct {
	project      string
	mro          string
	mroPath      []string
	transpileErr error
	digest       string // emission content digest; empty on transpile failure
}

// lintCorpusPrefix starts every corpus emission's project name, which no
// fixture's name does.
const lintCorpusPrefix = "corpus_"

// keywordInputRefusal is mro2nf's documented refusal of an entry input named
// with a Groovy or Nextflow keyword (#585). A corpus stage entry's input may
// be so named (Cell Ranger's LOGIC_NOT has `boolean`), though no pipeline
// that calls the stage is refused.
const keywordInputRefusal = "which Nextflow's params block refuses as a param name"

// lintEmissions are every fixture's invocation and every corpus
// invocation Martian accepts, each named by its project.
func lintEmissions(t *testing.T) []*lintEmission {
	t.Helper()

	fixtures := lintFixtures(t)
	emissions := make([]*lintEmission, 0, len(fixtures))

	for _, fx := range fixtures {
		if strings.HasPrefix(fx, lintCorpusPrefix) {
			t.Fatalf("fixture %s starts with %q, which names corpus emissions", fx, lintCorpusPrefix)
		}

		dir := filepath.Join(root, "testdata", fx)
		emissions = append(emissions, &lintEmission{project: fx, mro: filepath.Join(dir, "pipeline.mro"), mroPath: []string{dir}})
	}

	srcs, err := corpus.Sources(filepath.Join(root, "testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := corpus.Load(t.TempDir(), srcs)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range corpus.Accepted(entries) {
		emissions = append(emissions, &lintEmission{project: lintCorpusPrefix + corpus.Slug(e.Name), mro: e.Mro, mroPath: e.MroPath})
	}

	return emissions
}

// transpileLintEmissions transpiles every lintEmissions invocation into a
// directory of its project name under parent and digests each emission.
// Transpile failures are recorded, not fatal, so one broken invocation
// doesn't hide the verdicts for the rest.
func transpileLintEmissions(t *testing.T, parent string) []*lintEmission {
	t.Helper()

	emissions := lintEmissions(t)

	for _, e := range emissions {
		proj := filepath.Join(parent, e.project)
		if err := transpileWith(repoTools(), e.mroPath, e.mro, proj); err != nil {
			e.transpileErr = err

			continue
		}

		e.digest = treeDigest(t, proj)
	}

	return emissions
}

// transpiledProjects returns the project dirs of the invocations that
// transpiled, in lintEmissions (deterministic) order.
func transpiledProjects(emissions []*lintEmission) []string {
	var dirs []string

	for _, e := range emissions {
		if e.transpileErr == nil {
			dirs = append(dirs, e.project)
		}
	}

	return dirs
}

// assertPathIndependent re-transpiles every invocation into a directory of
// another name and requires a byte-identical emission: an absolute output
// path, a timestamp or an unstable map order in the emitter's output would
// change every task's inputs between transpiles of the same pipeline.
func assertPathIndependent(t *testing.T, emissions []*lintEmission) {
	t.Helper()

	again := t.TempDir()

	for _, e := range emissions {
		if e.transpileErr != nil {
			continue
		}

		proj := filepath.Join(again, e.project+"__again")
		if err := transpileWith(repoTools(), e.mroPath, e.mro, proj); err != nil {
			t.Errorf("re-transpile %s: %v", e.project, err)

			continue
		}

		if got := treeDigest(t, proj); got != e.digest {
			t.Errorf("%s: the emission into %s differs from the one into %s; it depends on its output path or is not deterministic",
				e.project, proj, e.project)
		}
	}
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

// lintEmissionAllowList is every top-level entry a real emission produces
// today. It backs the shared-context tripwire in assertNoProjectDirContext:
// main.nf includes stage.nf by its path beside it, and the modules of
// subworkflows/ by theirs, each including stage.nf and the others by
// theirs, nextflow.config includes conf/'s base and profile configs by
// theirs, the HealthOmics package files and the params schema are no
// Nextflow source, and nothing here is resolved from -project-dir.
var lintEmissionAllowList = []string{
	"_assets", "conf", "main.nf", "nextflow.config", "nextflow_schema.json", "package.sh", "parameter-template.json", "stage.nf", "subworkflows",
}

// assertNoProjectDirContext trips on any project-dir-anchored lint context,
// a lib/ or bin/ dir included: batch linting resolves such context from a
// single -project-dir, so an emission growing any top-level entry outside
// the known emission shape must be reviewed for whether it affects lint
// resolution (and this allow-list extended) before the batch verdicts can be
// trusted again.
func assertNoProjectDirContext(t *testing.T, parent string, projects []string) {
	t.Helper()

	for _, proj := range projects {
		entries, err := os.ReadDir(filepath.Join(parent, proj))
		if err != nil {
			t.Fatalf("read emission %s: %v", proj, err)
		}

		for _, e := range entries {
			if !slices.Contains(lintEmissionAllowList, e.Name()) {
				t.Errorf("emission %s has unexpected top-level entry %q; batch lint assumes no project-dir-anchored lint context — review whether nextflow lint resolves it from -project-dir before extending lintEmissionAllowList",
					proj, e.Name())
			}
		}
	}
}

// injectLintGuard copies a clean emission and appends a Groovy syntax error
// to its main.nf. The chunk it rides MUST report an error for it, proving on
// every run that the whole lint path — batch invocation, JSON parsing,
// per-project attribution — can actually surface a broken emission. A
// fixture's name holds no '-', so the guard's cannot collide with a
// project dir.
func injectLintGuard(t *testing.T, parent, rep string, n int) string {
	t.Helper()

	guard := fmt.Sprintf("zz-lint-guard-%d", n)
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

// batchLint lints the given emission directories (relative to parent) in a
// few `nextflow lint` invocations instead of one JVM per project — ~95% of a
// single-project lint is JVM boot. No project holds project-dir context
// (assertNoProjectDirContext), so the chunk's first project serves as
// -project-dir, and the JSON diagnostics carry project-relative paths
// that attribute each error back to its emission. EVERY chunk carries its own
// guard project (guard slots are reserved out of lintBatchSize), so a chunk
// whose invocation, JSON parse, or attribution silently broke cannot pass
// green. Returns error strings keyed by project dir name; projects absent
// from the map linted clean. Guard verdicts are consumed here, not returned.
func batchLint(t *testing.T, parent string, projects []string) map[string][]string {
	t.Helper()

	errsByProj := map[string][]string{}
	chunkNum := 0

	for chunk := range slices.Chunk(projects, lintBatchSize-1) {
		guard := injectLintGuard(t, parent, chunk[0], chunkNum)
		chunkNum++

		// slices.Concat, not append: chunk aliases projects' backing array, and
		// appending the guard in place would overwrite the next chunk's first
		// element.
		lintChunk(t, parent, slices.Concat(chunk, []string{guard}), errsByProj)

		assertGuardTeeth(t, guard, errsByProj[guard])
		delete(errsByProj, guard) // consumed; no fixture maps to a guard
	}

	return errsByProj
}

// lintChunk runs one `nextflow lint` invocation over chunk (relative to
// parent) and merges the attributed diagnostics into errsByProj.
func lintChunk(t *testing.T, parent string, chunk []string, errsByProj map[string][]string) {
	t.Helper()

	args := slices.Concat([]string{"lint", "-output", "json", "-project-dir", chunk[0]}, chunk)
	cmd := exec.Command("nextflow", args...)
	cmd.Dir = parent

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// A nonzero exit just means diagnostics were found; the JSON on
	// stdout is the verdict (progress lines go to stderr).
	out, runErr := cmd.Output()

	var report struct {
		Errors   []lintDiag `json:"errors"`
		Warnings []lintDiag `json:"warnings"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("nextflow lint batch produced no JSON (%v; run: %v):\nstdout:\n%s\nstderr:\n%s",
			err, runErr, out, stderr.Bytes())
	}

	if runErr != nil && len(report.Errors) == 0 {
		t.Fatalf("nextflow lint batch failed without diagnostics: %v\nstderr:\n%s", runErr, stderr.Bytes())
	}

	diags := report.Errors

	for _, w := range report.Warnings {
		w.Message = "warning: " + w.Message
		diags = append(diags, w)
	}

	for _, d := range diags {
		proj, rest, ok := attributeLintDiag(chunk, d.Filename)
		if !ok {
			// Never Fatalf: one oddly-reported path must not vaporize the
			// verdicts for the rest of the matrix — but it can't pass green
			// either, so surface the raw diagnostic as its own failure.
			t.Errorf("lint error with unattributable filename %q (no chunk project matches): %d:%d: %s",
				d.Filename, d.StartLine, d.StartColumn, d.Message)

			continue
		}

		errsByProj[proj] = append(errsByProj[proj],
			fmt.Sprintf("%s:%d:%d: %s", rest, d.StartLine, d.StartColumn, d.Message))
	}
}

// attributeLintDiag maps a diagnostic's filename to the chunk project it
// belongs to, plus the project-relative remainder. `nextflow lint` reports
// paths as passed, i.e. '<projdir>/…' relative to the invocation cwd, so the
// first path component is tried first; if the shape ever changes (an absolute
// path, say), any path still containing a chunk project's dir as a component
// attributes by suffix. ok=false means no chunk project matches (e.g. a bare
// 'main.nf' reported relative to the -project-dir) — the caller decides how
// loudly to fail. Returns (project dir, project-relative path, ok).
func attributeLintDiag(chunk []string, filename string) (string, string, bool) {
	clean := path.Clean(filepath.ToSlash(filename))

	if proj, rest, found := strings.Cut(clean, "/"); found && slices.Contains(chunk, proj) {
		return proj, rest, true
	}

	for _, member := range chunk {
		marker := "/" + member + "/"
		if _, after, ok := strings.CutLast(clean, marker); ok {
			return member, after, true
		}
	}

	return "", "", false
}

// assertGuardTeeth asserts a chunk's guard project reported exactly its
// injected main.nf syntax error. An empty verdict means the chunk's lint path
// lost its teeth (its real projects' green verdicts would be meaningless);
// an error outside main.nf means the diagnostics were misattributed.
func assertGuardTeeth(t *testing.T, guard string, errs []string) {
	t.Helper()

	if len(errs) == 0 {
		t.Fatalf("lint gate has no teeth: injected syntax error in %s produced no lint errors for its chunk", guard)
	}

	for _, e := range errs {
		if !strings.HasPrefix(e, "main.nf:") {
			t.Errorf("guard %s reported an error outside its injected main.nf breakage (misattribution?): %s", guard, e)
		}
	}
}

// TestNextflowLintAttribution unit-tests the diagnostic-to-project mapping:
// the as-passed '<projdir>/…' fast path, the tolerant suffix fallback, and
// the not-ok shapes that must degrade to a per-diagnostic failure instead of
// vaporizing the batch.
func TestNextflowLintAttribution(t *testing.T) {
	t.Parallel()

	chunk := []string{"fx", "fx_split", "zz-lint-guard-0"}

	cases := []struct {
		name, filename     string
		wantProj, wantRest string
		wantOK             bool
	}{
		{"as-passed prefix", "fx_split/main.nf", "fx_split", "main.nf", true},
		{"nested file", "fx/_assets/stage.nf", "fx", "_assets/stage.nf", true},
		{"dot-prefixed", "./fx/main.nf", "fx", "main.nf", true},
		{"absolute path", "/tmp/x/parent/zz-lint-guard-0/main.nf", "zz-lint-guard-0", "main.nf", true},
		{"bare basename", "main.nf", "", "", false},
		{"unknown project", "other/main.nf", "", "", false},
		{"empty", "", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj, rest, ok := attributeLintDiag(chunk, tc.filename)
			if proj != tc.wantProj || rest != tc.wantRest || ok != tc.wantOK {
				t.Errorf("attributeLintDiag(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.filename, proj, rest, ok, tc.wantProj, tc.wantRest, tc.wantOK)
			}
		})
	}
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

// requireNextflowLint asserts the installed Nextflow is at least the engine
// floor the generated nextflow.config declares, which is also the first
// release whose `nextflow lint` takes -project-dir. A too-old install FAILS
// loudly rather than silently skipping, so a stale local Nextflow cannot
// make the gate a no-op, and a floor bump in the emitter raises it.
func requireNextflowLint(t *testing.T) {
	t.Helper()

	cfg, err := os.ReadFile(filepath.Join(transpile(t, "file_min"), "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	floor := manifestFloor.FindSubmatch(cfg)
	if floor == nil {
		t.Fatalf("generated nextflow.config declares no manifest.nextflowVersion floor:\n%s", cfg)
	}

	out, err := exec.Command("nextflow", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("nextflow -version: %v\n%s", err, out)
	}

	m := regexp.MustCompile(`version\s+([0-9.]+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse nextflow version from:\n%s", out)
	}

	if slices.Compare(versionParts(t, string(m[1])), versionParts(t, string(floor[1]))) < 0 {
		t.Fatalf("nextflow %s is older than the engine floor %s", m[1], floor[1])
	}
}

// versionParts is a dotted release number's parts, "26.04.7" as [26 4 7].
func versionParts(t *testing.T, v string) []int {
	t.Helper()

	var parts []int

	for f := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("release %q: %v", v, err)
		}

		parts = append(parts, n)
	}

	return parts
}
