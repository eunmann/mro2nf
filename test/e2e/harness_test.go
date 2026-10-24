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
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// root is the repo root, resolved once in TestMain.
var root string

// shardSkip partitions the Nextflow-running subtests across CI runners. Every
// `nextflow run` is a fresh JVM, so the golden suite is JVM-boot-bound and
// core-throttled; sharding it across matrix jobs cuts wall time near-linearly.
// With E2E_SHARD_TOTAL=N>1, a subtest runs only on the shard its name hashes to
// (a stable FNV hash of the full t.Name(), so the partition is deterministic
// and disjoint — full coverage is the union of shards, enforced by the gate
// job). Unset or N<=1 runs everything, so local `make test-e2e` is unaffected.
// Gating here (rather than at each t.Run) covers every suite uniformly through
// the single chokepoint; the preceding transpile is milliseconds against a
// multi-second run, so the skipped work is negligible.
//
// A test admitted to this shard claims it for its subtests: they run here
// without hashing their own names. A test that does expensive shared work
// before its legs (TestMrpDiff's one mrp run per fixture) gates itself first,
// so that work runs on one shard and every leg runs beside it.
func shardSkip(tb testing.TB) {
	tb.Helper()

	total, err := strconv.Atoi(os.Getenv("E2E_SHARD_TOTAL"))
	if err != nil || total <= 1 {
		return
	}

	index, err := strconv.Atoi(os.Getenv("E2E_SHARD_INDEX"))
	if err != nil || index < 0 || index >= total {
		tb.Fatalf("E2E_SHARD_INDEX=%q invalid for E2E_SHARD_TOTAL=%d", os.Getenv("E2E_SHARD_INDEX"), total)

		return
	}

	name := tb.Name()
	if claimedAncestor(name) {
		return
	}

	if shardOf(name, total) != index {
		tb.Skipf("shard %d/%d: %s runs in another shard", index, total, name)

		return
	}

	shardClaims.Store(name, true)
}

// shardClaims holds the names of the tests shardSkip admitted to this shard.
var shardClaims sync.Map

// claimedAncestor reports whether the test name, or a test it is nested
// under, was admitted to this shard.
func claimedAncestor(name string) bool {
	for {
		if _, ok := shardClaims.Load(name); ok {
			return true
		}

		i := strings.LastIndex(name, "/")
		if i < 0 {
			return false
		}

		name = name[:i]
	}
}

// shardOf is the shard a test name hashes to: a stable FNV hash, so the
// partition is deterministic and disjoint across runners.
func shardOf(name string, total int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))

	return int(h.Sum32() % uint32(total))
}

// TestMain builds the transpiler + shim once and pins the Nextflow environment
// the shell harnesses used (no ANSI redraw, no version check, small JVM).
func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "getwd:", err)
		os.Exit(1)
	}

	root = filepath.Dir(filepath.Dir(wd)) // test/e2e -> repo root

	mustSetenv("NXF_ANSI_LOG", "false")
	mustSetenv("NXF_DISABLE_CHECK_LATEST", "true")

	if os.Getenv("NXF_OPTS") == "" {
		mustSetenv("NXF_OPTS", "-Xms256m -Xmx1g -XX:+UseSerialGC")
	}

	// Martian's runtime, which the call-graph drift guard runs in-process,
	// finds jobmanagers/ beside MARTIAN_BASE; the test binary's own dir has
	// none.
	if bin, err := lookupMartianBin(); err == nil && os.Getenv("MARTIAN_BASE") == "" {
		mustSetenv("MARTIAN_BASE", bin)
	}

	os.Exit(m.Run())
}

// mustSetenv sets an env var in TestMain (where t.Setenv is unavailable). The
// pinned Nextflow environment is load-bearing for every test, so a failure to
// set it aborts the run instead of silently proceeding unpinned.
func mustSetenv(key, value string) {
	if err := os.Setenv(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "setenv %s: %v\n", key, err)
		os.Exit(1)
	}
}

// requireTools skips the test when a prerequisite is missing, or fails it
// under E2E_REQUIRE=1 (missingPrereqf).
func requireTools(t *testing.T, tools ...string) {
	t.Helper()

	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			missingPrereqf(t, "%s not found", tool)
		}
	}
}

// missingPrereqf skips the test for an absent prerequisite — unless
// E2E_REQUIRE=1 (set in CI), where a missing tool must FAIL, not silently
// green-skip the gate.
func missingPrereqf(tb testing.TB, format string, args ...any) {
	tb.Helper()

	if os.Getenv("E2E_REQUIRE") == "1" {
		tb.Fatalf(format+" (required by E2E_REQUIRE=1)", args...)

		return
	}

	tb.Skipf(format, args...)
}

var buildOnce sync.Once

// builtBinaries are the binaries `make build` writes at the repo root, each
// with its package: mrjob is the pinned Martian module's.
var builtBinaries = map[string]string{
	"mro2nf": "./cmd/mro2nf",
	"mre":    "./cmd/mre",
	"mrjob":  "github.com/martian-lang/martian/cmd/mrjob",
}

// buildBinaries builds builtBinaries at the repo root exactly once per test
// process, static and path-free like `make build`, so the suites run (and the
// docker images bake) the binary a user builds.
func buildBinaries(t *testing.T) {
	t.Helper()

	buildOnce.Do(func() {
		for _, bin := range slices.Sorted(maps.Keys(builtBinaries)) {
			cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(root, bin), builtBinaries[bin])
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build %s: %v\n%s", bin, err, out)
			}
		}
	})
}

// transpile runs mro2nf on a testdata fixture into a fresh temp project dir,
// whose stages run under the pinned Martian mrjob buildBinaries builds.
// extra flags are appended before the .mro argument.
func transpile(t *testing.T, fixture string, extra ...string) string {
	t.Helper()

	return transpileDir(t, filepath.Join(root, "testdata", fixture), extra...)
}

// transpileDir is transpile for a fixture at an arbitrary directory (e.g. the
// bench/ pipelines).
func transpileDir(t *testing.T, dir string, extra ...string) string {
	t.Helper()

	return transpileMRO(t, dir, "pipeline.mro", extra...)
}

// transpileMRO is transpileDir for a named entry .mro inside the fixture dir —
// used by fixtures that ship several top-level invocations of one pipeline
// (e.g. the bench two-width variants pipeline_w4.mro / pipeline_w16.mro).
func transpileMRO(t *testing.T, dir, mro string, extra ...string) string {
	t.Helper()
	buildBinaries(t)

	proj := t.TempDir()
	if err := transpileInto(repoTools(), dir, mro, proj, extra...); err != nil {
		t.Fatalf("transpile %s: %v", dir, err)
	}

	return proj
}

// transpileRefused are the fixtures mro2nf must refuse to transpile, each
// with the reason no project may exist to run: mrp refuses it before any
// stage runs, or Nextflow could not parse the project. Each is an
// mrpOutcomeCases entry, and TestNextflowLint requires the refusal in place
// of a lint verdict.
var transpileRefused = map[string]string{
	"keyword_input": "Nextflow's params block refuses a Groovy keyword as a param name, quoted or not (#585)",
}

// toolchain is the mro2nf binary a test transpiles with and the mre binary
// the generated project runs.
type toolchain struct {
	mro2nf string
	mre    string
}

// repoTools is the toolchain buildBinaries builds at the repo root.
func repoTools() toolchain {
	return toolchain{mro2nf: filepath.Join(root, "mro2nf"), mre: filepath.Join(root, "mre")}
}

// transpileInto runs tools' mro2nf for the fixture mro under dir, emitting
// into proj. It returns the failure (with the tool output) instead of failing
// the test, so batch callers can attribute a broken combo without aborting the
// rest. The binaries must exist: buildBinaries builds repoTools. Every stage
// runs under the real mrjob beside tools' mre, as mrp runs py and comp
// stages.
func transpileInto(tools toolchain, dir, mro, proj string, extra ...string) error {
	args := slices.Concat([]string{"-o", proj, "-mropath", dir}, extra, []string{filepath.Join(dir, mro)})

	cmd := exec.Command(tools.mro2nf, args...)
	cmd.Dir = root

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mro2nf: %w\n%s", err, out)
	}

	if tools.mre != repoTools().mre {
		projectMre.Store(proj, tools.mre)
	}

	return nil
}

// projectMre maps each project transpileInto emitted for another mre than
// the repo's, which every run finds on PATH, to that mre, which its runs
// name with the project's mro2nf_mre param.
var projectMre sync.Map

// launchArgs are the args every run of proj gets: the mre it runs, when
// not the repo's.
func launchArgs(proj string) []string {
	v, _ := projectMre.Load(proj)
	if mre, ok := v.(string); ok {
		return []string{"--mro2nf_mre", mre}
	}

	return nil
}

// runNextflow runs `nextflow run main.nf <args...>` in proj, always capturing
// the combined output to proj/run.log and tracing every task attempt (to the
// caller's -with-trace file, or a temp one). On error the log tail is attached
// to the returned error so failures are never silent. A run that succeeds
// still fails if any attempt in its trace did not complete: the generated
// config retries failed tasks, so a flaky failure that passes on retry would
// otherwise keep the suite green (#269).
func runNextflow(t *testing.T, proj string, args ...string) error {
	t.Helper()

	return runNextflowJVM(t, proj, "", args...)
}

// runNextflowJVM is runNextflow with jvm appended to the driver's NXF_OPTS,
// after the harness's own options, so a flag in it wins.
func runNextflowJVM(t *testing.T, proj, jvm string, args ...string) error {
	t.Helper()

	args, trace, err := traceArgs(proj, args, filepath.Join(t.TempDir(), "trace.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if err := execNextflowJVM(t, proj, jvm, args); err != nil {
		return err
	}

	return checkAttempts(readTrace(t, trace))
}

// runNextflowAllowingFailedAttempts is runNextflow for a test that makes
// tasks fail on purpose — a retry, an assertion, a run that must fail — so
// its own assertions on the run and the trace decide, not the failed-attempt
// check.
func runNextflowAllowingFailedAttempts(t *testing.T, proj string, args ...string) error {
	t.Helper()

	return execNextflow(t, proj, args)
}

// errTraceArg reports a -with-trace flag runNextflow cannot locate a file for.
var errTraceArg = errors.New("-with-trace needs a file name")

// traceArgs returns args with a -with-trace flag and the trace file the run
// will write: the caller's own -with-trace file (relative to proj, where
// Nextflow runs), or else fallback, appended as a new flag.
func traceArgs(proj string, args []string, fallback string) ([]string, string, error) {
	for i, a := range args {
		if a != "-with-trace" {
			continue
		}

		if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
			return nil, "", fmt.Errorf("runNextflow %v: %w", args, errTraceArg)
		}

		if filepath.IsAbs(args[i+1]) {
			return args, args[i+1], nil
		}

		return args, filepath.Join(proj, args[i+1]), nil
	}

	return append(slices.Clone(args), "-with-trace", fallback), fallback, nil
}

// errFailedAttempt reports a task attempt that did not complete in a run
// that succeeded.
var errFailedAttempt = errors.New("task attempt did not complete")

// checkAttempts fails on any trace row that is neither COMPLETED nor CACHED
// (a FAILED attempt a retry recovered, an ABORTED one), naming each.
func checkAttempts(rows []traceRow) error {
	var bad []string

	for _, r := range rows {
		if r.Status != "COMPLETED" && r.Status != "CACHED" {
			bad = append(bad, fmt.Sprintf("%s %s (exit %d)", r.Name, r.Status, r.Exit))
		}
	}

	if len(bad) > 0 {
		return fmt.Errorf("%w: the run succeeded but %d attempt(s) failed, which a retry hid: %s",
			errFailedAttempt, len(bad), strings.Join(bad, "; "))
	}

	return nil
}

// execNextflow runs Nextflow in proj with args as given.
func execNextflow(t *testing.T, proj string, args []string) error {
	t.Helper()

	return execNextflowJVM(t, proj, "", args)
}

// execNextflowJVM is execNextflow with jvm appended to the driver's NXF_OPTS.
func execNextflowJVM(t *testing.T, proj, jvm string, args []string) error {
	t.Helper()
	shardSkip(t)

	cmd := exec.Command("nextflow", slices.Concat([]string{"run", "main.nf"}, args, launchArgs(proj))...)
	cmd.Dir = proj
	// Every run pays a fresh JVM, so startup dominates suite time: skip the
	// remote version/plugin check (no fixture declares plugins), drop ANSI
	// rendering, and cap JIT tiering + heap — the pipelines are trivial
	// compute, and the smaller heap lets more runs overlap under -parallel.
	// An inherited NXF_OPTS (proxy, truststore) is PREPENDED so it survives:
	// within one NXF_OPTS the later flags win, keeping the heap cap. An
	// explicitly exported NXF_OFFLINE is honored (fresh-launcher bootstrap
	// needs the network once).
	// The tasks find the repo's mre on PATH, as a project's tasks do by
	// default.
	env := append(os.Environ(),
		"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NXF_ANSI_LOG=false",
		"NXF_OPTS="+strings.TrimSpace(os.Getenv("NXF_OPTS")+" -XX:TieredStopAtLevel=1 -Xms64m -Xmx512m "+jvm),
	)
	if os.Getenv("NXF_OFFLINE") == "" {
		env = append(env, "NXF_OFFLINE=true")
	}

	cmd.Env = env

	keepRunLogsOnFailure(t, proj)

	out, err := cmd.CombinedOutput()

	_ = os.WriteFile(filepath.Join(proj, "run.log"), out, 0o644)

	return nextflowRunError(err, out)
}

// runLogTailLines is how much of a failed run's output its error carries:
// enough for Nextflow's whole error report, whose "Command error" block
// follows the failed task's command, exit status and output.
const runLogTailLines = 40

// nextflowRunError is the error for a `nextflow run` that returned err, with
// the end of its output out attached.
func nextflowRunError(err error, out []byte) error {
	if err != nil {
		return fmt.Errorf("nextflow run: %w\n%s", err, tail(out, runLogTailLines))
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

func writeFileT(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// goldenJSON asserts the JSON document at gotPath, a pipeline_outs.json of
// fixture read as the published-output comparison reads it, equals the golden
// at goldenPath, number tokens and key order included.
func goldenJSON(t *testing.T, fixture, gotPath, goldenPath string) {
	t.Helper()

	got, err := normalizeOutsFile(gotPath, nfOutsRewrite(mustAllowlist(t), fixture))
	if err != nil {
		t.Fatal(err)
	}

	want, err := normalizeOutsFile(goldenPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(indentJSON(want), indentJSON(got)); diff != "" {
		t.Errorf("pipeline outs mismatch (-golden +got):\n%s", diff)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := decodeJSONNumber(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// decodeJSONNumber decodes JSON with UseNumber so numbers keep their exact token
// (json.Number), not float64: a plain json.Unmarshal would make a comparator
// blind to an int-vs-float regression (21 vs 21.0)
// and to >2^53 integer precision loss (the literals_edge / src_args fixtures
// target exactly those ranges). The producer keeps the token via UseNumber
// (cmd/mre/bundle.go rawToMap); a comparison must decode BOTH sides this way, so
// use it for every want/got side, not just readJSON. It also rejects trailing
// data so a malformed multi-document output still fails loudly (#188).
func decodeJSONNumber(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	if err := dec.Decode(v); err != nil {
		return err
	}

	if dec.More() {
		return errTrailingJSON
	}

	return nil
}

// errTrailingJSON reports a comparator input file that is a valid JSON value
// followed by more data (a malformed multi-document output) — kept loud rather
// than silently accepting only the first document.
var errTrailingJSON = errors.New("trailing data after JSON value")

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// traceRow is one task attempt from a `-with-trace` TSV file. Hash is the
// work dir prefix (`ab/cdef12`).
type traceRow struct {
	Hash   string
	Name   string
	Status string
	Exit   int
}

// readTrace parses a Nextflow trace file into typed rows (replacing the awk
// column indexing that already caused one harness bug).
func readTrace(t *testing.T, path string) []traceRow {
	t.Helper()

	rows, err := parseTrace(path)
	if err != nil {
		t.Fatal(err)
	}

	return rows
}

// parseTrace is readTrace returning its error.
func parseTrace(path string) ([]traceRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trace %s: %w", path, err)
	}

	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) == 0 {
		return nil, nil
	}

	cols := map[string]int{}
	for i, h := range strings.Split(string(lines[0]), "\t") {
		cols[h] = i
	}

	rows := make([]traceRow, 0, len(lines)-1)

	for _, line := range lines[1:] {
		f := strings.Split(string(line), "\t")

		row := traceRow{Hash: f[cols["hash"]], Name: f[cols["name"]], Status: f[cols["status"]], Exit: -1}
		if n, err := strconv.Atoi(f[cols["exit"]]); err == nil {
			row.Exit = n
		}

		rows = append(rows, row)
	}

	return rows, nil
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
