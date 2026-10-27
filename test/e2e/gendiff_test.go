//go:build e2e

package e2e

// The generative differential (#350): random well-typed Martian programs from
// test/gen run under real mrp and under mro2nf + Nextflow, and face the same
// checks TestMrpDiff and TestMrpDiffFailureParity apply to the fixtures: the
// published tree, the _outs JSON and every stage invocation's _args, _outs
// and _jobinfo allocation when both runs succeed, the run outcome when either fails. A divergence is
// written as a fixture directory, shrunk to a minimal program first unless
// GEN_SHRINK_EVALS is 0.
//
// Each program runs at its literal invocation, which mostly binds empty
// collections, and again at its witness invocation (cg.Source.Witness),
// which forks every entry collection, nested ones at different widths; a
// witness run's findings are under the leg <leg>@witness. A run in which
// mrp ran a job on bindings it failed to resolve is no test: mrp ran it on
// nulls in their place (#606).
//
// A generated program is no fixture, so every comparator rule of the
// allowlist (divergences.json) applies to it, an issue-tracked one included,
// since a known divergence is no new finding; but none is reported as
// excusing nothing in it, and the ABI and outcome matchers, which name the
// fixtures or cases they apply to, never apply.
//
// Environment:
//
//	GEN_SEEDS          unset: the checked-in genSmokeSeeds. Else a start seed,
//	                   "random" (a start drawn and logged), or a comma list.
//	GEN_COUNT          seeds from the start (default 20).
//	GEN_MRO2NF_BIN,    prebuilt binaries to test instead of the repo build,
//	GEN_MRE_BIN        e.g. an older commit's; mre runs the mrjob beside it.
//	GEN_ARTIFACTS      where shrunk fixtures go (default artifacts/gen-diff).
//	GEN_SHRINK_EVALS   shrink budget in program runs (default 150, or 0 for
//	                   the smoke seeds; 0 skips).
//	GEN_SHRINK_PARALLEL candidates run at once while shrinking (default 3).
//	GEN_SHRINK_KEEP    regexp of finding ids ("<leg> <key>") the shrink must
//	                   keep, to isolate one of several divergences.
//	GEN_FIXTURES       comma list of fixture dirs (such as shrunk ones) to
//	                   rerun instead of generating programs.
//	GEN_MRP_TIMEOUT    seconds an mrp run may take (default 180).
//	GEN_BUDGET         minutes after which no further seed starts; those
//	                   left skip (default 0: no limit).
//	GEN_FINDINGS       file each diverging seed is appended to as a JSON
//	                   line instead of failing the test, so the run fails
//	                   only on a harness error (default: fail the test).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/test/divergence"
	"github.com/eunmann/mro2nf/test/gen"
	"github.com/google/go-cmp/cmp"
)

// genSmokeSeeds are the seeds every PR runs: between them their programs
// reach every construct and corpus value test/gen covers (gen.Features) at
// least once, mrp runs each of them, and mro2nf matches mrp on every leg.
var genSmokeSeeds = []uint64{1, 2, 3, 4, 6, 8, 9, 10, 11, 12, 13, 14, 15, 22, 43, 108, 154, 192, 193, 246}

// genLegs are the transpiles every program runs under.
var genLegs = []genLeg{{name: "default"}}

type genLeg struct {
	name string
}

// genEnv is how TestGenDiff runs: the oracle, the binaries under test, the
// allowlist entries that apply to a generated program, the legs, where shrunk
// fixtures go and the shrink budget.
type genEnv struct {
	martian   string
	tools     toolchain
	prebuilt  bool
	allow     divergence.Allowlist
	legs      []genLeg
	artifacts string
	shrink    gen.ShrinkOptions
	keep      *regexp.Regexp
	// mrpTimeout bounds one mrp run: mrp livelocks on some valid programs
	// (a nested fork it re-completes forever), which are then no test.
	mrpTimeout time.Duration
	// deadline is when GEN_BUDGET is spent; zero for no budget.
	deadline time.Time
	// findings is the GEN_FINDINGS file, or "" to fail on a divergence.
	findings string
}

// divergenceReporter is how runGenSeed reports a divergence: as a test
// failure, or, with GEN_FINDINGS, as a log line beside the record.
func (e genEnv) divergenceReporter(t *testing.T) func(format string, args ...any) {
	t.Helper()

	if e.findings == "" {
		return t.Errorf
	}

	return t.Logf
}

// genDivergence is one GEN_FINDINGS line: a diverging seed, the ids of its
// findings and its fixture's dir name under GEN_ARTIFACTS.
type genDivergence struct {
	Seed     uint64   `json:"seed"`
	Findings []string `json:"findings"`
	Fixture  string   `json:"fixture"`
}

// recordDivergence appends the seed's divergence to GEN_FINDINGS, if set.
// Each line is one O_APPEND write, which Linux applies whole, so seeds that
// diverge at once cannot interleave their lines.
func (e genEnv) recordDivergence(seed uint64, v genVerdict, fixture string) error {
	if e.findings == "" {
		return nil
	}

	d := genDivergence{Seed: seed, Fixture: filepath.Base(fixture)}
	for _, f := range v.findings {
		d.Findings = append(d.Findings, f.id())
	}

	line, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode divergence: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(e.findings), 0o755); err != nil {
		return fmt.Errorf("create findings dir: %w", err)
	}

	f, err := os.OpenFile(e.findings, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open findings: %w", err)
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()

		return fmt.Errorf("append divergence: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close findings: %w", err)
	}

	return nil
}

// budgetSpent reports whether a seed starting at now is past the budget.
func (e genEnv) budgetSpent(now time.Time) bool {
	return !e.deadline.IsZero() && now.After(e.deadline)
}

// TestGenDiff runs each selected seed's program under mrp and mro2nf and
// fails on any divergence, naming the seed and its fixture.
func TestGenDiff(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	env := loadGenEnv(t)

	if fixtures := os.Getenv("GEN_FIXTURES"); fixtures != "" {
		for dir := range strings.SplitSeq(fixtures, ",") {
			t.Run(filepath.Base(dir), func(t *testing.T) {
				t.Parallel()
				runGenFixture(t, env, rootPath(dir))
			})
		}

		return
	}

	seeds, random := genSeeds(t)

	for _, seed := range seeds {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			if env.budgetSpent(time.Now()) {
				t.Skipf("GEN_BUDGET spent before seed %d started", seed)
			}

			runGenSeed(t, env, seed, random)
		})
	}
}

// TestGenBudgetSpent pins GEN_BUDGET's cutoff: a seed starts until the
// deadline passes, and without a budget always starts.
func TestGenBudgetSpent(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name     string
		deadline time.Time
		want     bool
	}{
		{"no budget", time.Time{}, false},
		{"before the deadline", now.Add(time.Minute), false},
		{"past the deadline", now.Add(-time.Minute), true},
	}

	for _, tc := range cases {
		if got := (genEnv{deadline: tc.deadline}).budgetSpent(now); got != tc.want {
			t.Errorf("%s: budgetSpent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStageTasksVerdict pins the stage-task gate on a fake run: the tasks
// of the stage module's aliases count, each once however many attempts it
// took, plumbing tasks do not, and a count unequal to mrp's jobs is a
// finding.
func TestStageTasksVerdict(t *testing.T) {
	proj, ps := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(proj, "main.nf"), "include { STAGE as N0_A; STAGE_SPLIT as N1_B_SP } from './modules/stage.nf'\n")
	writeTestFile(t, filepath.Join(proj, "subworkflows", "P.nf"), "include { STAGE_MAIN as N1_B_MN } from '../modules/stage.nf'\n")
	writeTestFile(t, filepath.Join(proj, "trace.txt"), "hash\tname\tstatus\texit\n"+
		"aa/01\tPLAN\tCOMPLETED\t0\n"+
		"aa/02\tP:N0_A (1)\tFAILED\t1\n"+
		"aa/03\tP:N0_A (1)\tCOMPLETED\t0\n"+
		"aa/04\tP:N1_B_SP (1)\tCOMPLETED\t0\n"+
		"aa/05\tP:SUB:N1_B_MN (1)\tCOMPLETED\t0\n"+
		"aa/06\tP:SUB:N1_B_MN (2)\tCOMPLETED\t0\n"+
		"aa/07\tP:GATHER (1)\tCOMPLETED\t0\n")

	for i := range 4 {
		writeTestFile(t, filepath.Join(ps, "P", "fork0", fmt.Sprintf("chnk%d", i), "_jobinfo"), "{}")
	}

	v, err := stageTasksVerdict("default", ps, proj)
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff([]string{"default: 4 stage tasks, mrp ran 4 jobs"}, v.notes); d != "" || len(v.findings) > 0 {
		t.Errorf("4 tasks, 4 jobs: findings %v, notes (-want +got):\n%s", v.findings, d)
	}

	writeTestFile(t, filepath.Join(ps, "P", "fork0", "join", "_jobinfo"), "{}")

	v, err = stageTasksVerdict("default", ps, proj)
	if err != nil {
		t.Fatal(err)
	}

	if keys := findingKeys(v.findings); keys != "tasks" {
		t.Errorf("4 tasks, 5 jobs: findings %v, want one tasks finding", v.findings)
	}
}

// TestGenFindingsRecordDivergences pins GEN_FINDINGS's half of the exit
// split: a divergence leaves the test passing, and every diverging seed is
// one whole line of the file even when many diverge at once.
func TestGenFindingsRecordDivergences(t *testing.T) {
	env := genEnv{findings: filepath.Join(t.TempDir(), "gen-diff", "divergences.jsonl")}
	v := genVerdict{findings: []genFinding{{leg: "default", key: "outs:r0"}, {leg: "default", key: "abi:ST0 main"}}}

	if !t.Run("divergence", func(t *testing.T) { env.divergenceReporter(t)("seed %d diverges", 7) }) {
		t.Error("a divergence failed the test under GEN_FINDINGS")
	}

	const seeds = 50

	var wg sync.WaitGroup
	for seed := range uint64(seeds) {
		wg.Go(func() {
			if err := env.recordDivergence(seed, v, "/artifacts/gen-diff/gen_0123456789ab"); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	data, err := os.ReadFile(env.findings)
	if err != nil {
		t.Fatal(err)
	}

	got := map[uint64]genDivergence{}

	for line := range strings.Lines(string(data)) {
		var d genDivergence
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}

		got[d.Seed] = d
	}

	want := genDivergence{Seed: 3, Findings: []string{"default outs:r0", "default abi:ST0 main"}, Fixture: "gen_0123456789ab"}
	if diff := cmp.Diff(want, got[3]); len(got) != seeds || diff != "" {
		t.Errorf("%d seeds recorded, want %d; seed 3 (-want +got):\n%s", len(got), seeds, diff)
	}
}

// TestGenDiffHarnessErrorFails pins the other half: under GEN_FINDINGS a
// harness error, here mrp missing, still fails the run. It reruns this test
// binary on TestGenDiff alone.
func TestGenDiffHarnessErrorFails(t *testing.T) {
	requireTools(t, "nextflow", "java", "python3")

	cmd := exec.Command(os.Args[0], "-test.run=^TestGenDiff$", "-test.count=1")
	cmd.Dir = filepath.Join(root, "test", "e2e")
	cmd.Env = append(os.Environ(), "E2E_REQUIRE=1", "MARTIAN_BIN="+t.TempDir(),
		"GEN_SEEDS=1", "GEN_COUNT=1", "GEN_FINDINGS="+filepath.Join(t.TempDir(), "divergences.jsonl"))

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(string(out), errNoMrp.Error()) {
		t.Errorf("TestGenDiff without mrp: err %v, want a failed run naming %q:\n%s", err, errNoMrp, out)
	}
}

// TestGenDiffSkipsRefusedShape pins that a program mro2nf refuses as a call
// graph shape it does not wire, or for an entry input a keyword names
// (keyword-input), is no test, rather than a divergence filed against every
// refusal the generator or the corpus reaches.
func TestGenDiffSkipsRefusedShape(t *testing.T) {
	buildBinaries(t)

	for name, mro := range map[string]string{"shape": refusedShapeMRO, "keyword_input": keywordInputMRO} {
		t.Run(name, func(t *testing.T) {
			dir, work := t.TempDir(), t.TempDir()
			writeTestFile(t, filepath.Join(dir, "pipeline.mro"), mro)

			errs, skip := transpileGenLegs(genEnv{tools: repoTools(), legs: genLegs}, dir, work)
			if skip == "" {
				t.Errorf("transpileGenLegs = %v, want a skip for the refusal", errs)
			}
		})
	}
}

// keywordInputMRO calls a stage whose input is named with a Groovy keyword,
// as Cell Ranger's LOGIC_NOT is.
const keywordInputMRO = `
stage NOT(in bool boolean, out bool not, src py "stages/not",)

call NOT(boolean = false,)
`

// TestGenDiffSkipsPlanRefusal pins planRefusal: a run whose PLAN failed
// refusing the shape is no test, as a refusal at transpile time is, and a
// PLAN that failed otherwise, or a stage that failed, refuses nothing.
func TestGenDiffSkipsPlanRefusal(t *testing.T) {
	const refused = "mre: plan: unsupported call graph shape: TOP.W has a disable binding over its forks"

	cases := []struct {
		name, trace, stderr, want string
	}{
		{"refused", "60/4010e7\tPLAN\tFAILED\t1\n", refused, refused},
		{"plan_failed_otherwise", "60/4010e7\tPLAN\tFAILED\t1\n", "mre: plan: read values.json: no such file", ""},
		{"stage_failed", "60/4010e7\tPLAN\tCOMPLETED\t0\n61/000000\tP:N0_W (1)\tFAILED\t1\n", refused, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proj := t.TempDir()
			writeTestFile(t, filepath.Join(proj, "trace.txt"), "hash\tname\tstatus\texit\n"+c.trace)
			writeTestFile(t, filepath.Join(proj, "work", "60", "4010e7aa", ".command.err"), c.stderr+"\n")
			writeTestFile(t, filepath.Join(proj, "work", "61", "000000aa", ".command.err"), c.stderr+"\n")

			got, err := planRefusal(proj)
			if err != nil || got != c.want {
				t.Errorf("planRefusal = %q, %v; want %q", got, err, c.want)
			}
		})
	}

	if got, err := planRefusal(t.TempDir()); err != nil || got != "" {
		t.Errorf("planRefusal of a run with no trace = %q, %v; want none", got, err)
	}
}

// refusedShapeMRO zips a disable gate from one stage's outs into a map call
// another stage's outs fork, which mro2nf refuses.
const refusedShapeMRO = `
stage SRC(out bool[] skips, src py "stages/src",)
stage OTHER(out int[] xs, src py "stages/other",)
stage WORK(in int x, out int y, src py "stages/work",)

pipeline INNER(in int x, in bool skip, out int y,)
{
    call WORK(x = self.x,) using (disabled = self.skip,)
    return (y = WORK.y,)
}

pipeline P(out int[] ys,)
{
    call SRC()
    call OTHER()
    map call INNER(x = split OTHER.xs, skip = split SRC.skips,)
    return (ys = INNER.y,)
}

call P()
`

// TestGenDiffHarnessErrorFailsSkipsWithoutTools pins that test's guard:
// without TestGenDiff's tools and E2E_REQUIRE it skips, as TestGenDiff does,
// rather than failing on the tool its E2E_REQUIRE=1 rerun finds missing.
func TestGenDiffHarnessErrorFailsSkipsWithoutTools(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestGenDiffHarnessErrorFails$", "-test.count=1", "-test.v")
	cmd.Dir = filepath.Join(root, "test", "e2e")
	cmd.Env = append(os.Environ(), "PATH="+t.TempDir(), "E2E_REQUIRE=")

	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- SKIP: TestGenDiffHarnessErrorFails") {
		t.Errorf("TestGenDiffHarnessErrorFails without its tools: err %v, want a skip:\n%s", err, out)
	}
}

// TestGenSmokeSeedsCoverTheGrammar fails when the smoke seeds miss a
// construct or corpus value that some program among the first seeds uses, so
// the PR run keeps exercising everything the generator does.
func TestGenSmokeSeedsCoverTheGrammar(t *testing.T) {
	t.Parallel()

	reach, smoke := map[string]bool{}, map[string]bool{}

	for seed := uint64(1); seed <= 300; seed++ {
		p, err := gen.Generate(seed)
		if err != nil {
			t.Fatal(err)
		}

		maps.Copy(reach, gen.Features(p))

		if slices.Contains(genSmokeSeeds, seed) {
			maps.Copy(smoke, gen.Features(p))
		}
	}

	for _, f := range slices.Sorted(maps.Keys(reach)) {
		if !smoke[f] {
			t.Errorf("no smoke seed generates %s", f)
		}
	}
}

func runGenSeed(t *testing.T, env genEnv, seed uint64, random bool) {
	t.Helper()

	prog, err := gen.Generate(seed)
	if err != nil {
		t.Fatal(err)
	}

	v, err := evalGenProgram(t, env, prog)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range v.notes {
		t.Log(n)
	}

	switch {
	case v.skip != "" && !random:
		t.Fatalf("smoke seed %d is no test (%s); replace it in genSmokeSeeds", seed, v.skip)
	case v.skip != "":
		t.Skipf("seed %d is no test: %s", seed, v.skip)
	case len(v.findings) == 0:
		return
	}

	report := env.divergenceReporter(t)
	report("seed %d diverges from mrp (rerun: GEN_SEEDS=%d GEN_COUNT=1 make gen-diff):\n%s", seed, seed, v)

	small, sv, kind := prog, v, "unshrunk fixture (the rerun line shrinks)"
	if env.shrink.MaxEvals > 0 {
		small, sv = shrinkGenProgram(t, env, prog, v)
		kind = "shrunk fixture"
	}

	dir, err := writeGenFixture(t, env, small, sv)
	if err != nil {
		t.Fatal(err)
	}

	report("seed %d: %s %s (%s):\n%s", seed, kind, dir, gen.Size(small), sv)

	if err := env.recordDivergence(seed, v, dir); err != nil {
		t.Fatal(err)
	}
}

// runGenFixture reruns a fixture a shrink wrote, from a copy so the runs
// leave nothing in it.
func runGenFixture(t *testing.T, env genEnv, dir string) {
	t.Helper()

	work := t.TempDir()
	fixture := filepath.Join(work, "fixture")

	if err := os.CopyFS(fixture, os.DirFS(dir)); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	v, err := evalGenFixture(t, env, fixture, work)

	switch {
	case err != nil:
		t.Fatal(err)
	case v.skip != "":
		t.Skipf("%s is no test: %s", dir, v.skip)
	case len(v.findings) > 0:
		t.Errorf("%s diverges from mrp:\n%s", dir, v)
	}
}

// defaultShrinkEvals is 0 for the smoke seeds, which only detect: a shrink
// outlasts a PR's e2e job, which then reports nothing, and the rerun line
// each divergence logs shrinks.
func defaultShrinkEvals() int {
	if os.Getenv("GEN_SEEDS") == "" {
		return 0
	}

	return 150
}

func loadGenEnv(t *testing.T) genEnv {
	t.Helper()

	env := genEnv{
		martian:   martianBin(t),
		tools:     repoTools(),
		allow:     genAllowlist(t),
		artifacts: rootPath(envOr("GEN_ARTIFACTS", filepath.Join("artifacts", "gen-diff"))),
		shrink: gen.ShrinkOptions{
			MaxEvals: envInt(t, "GEN_SHRINK_EVALS", defaultShrinkEvals()),
			Parallel: envInt(t, "GEN_SHRINK_PARALLEL", 3),
		},
		mrpTimeout: time.Duration(envInt(t, "GEN_MRP_TIMEOUT", 180)) * time.Second,
	}

	if findings := os.Getenv("GEN_FINDINGS"); findings != "" {
		env.findings = rootPath(findings)
	}

	if budget := envInt(t, "GEN_BUDGET", 0); budget > 0 {
		env.deadline = time.Now().Add(time.Duration(budget) * time.Minute)
	}

	if keep := os.Getenv("GEN_SHRINK_KEEP"); keep != "" {
		re, err := regexp.Compile(keep)
		if err != nil {
			t.Fatalf("GEN_SHRINK_KEEP: %v", err)
		}

		env.keep = re
	}

	mro2nf, mre := os.Getenv("GEN_MRO2NF_BIN"), os.Getenv("GEN_MRE_BIN")

	switch {
	case mro2nf == "" && mre == "":
		buildBinaries(t)
	case mro2nf == "" || mre == "":
		t.Fatal("set GEN_MRO2NF_BIN and GEN_MRE_BIN together")
	default:
		env.tools = toolchain{mro2nf: rootPath(mro2nf), mre: rootPath(mre)}
		env.prebuilt = true

		for _, bin := range []string{env.tools.mro2nf, env.tools.mre} {
			if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
				t.Fatalf("%s is not an executable (err %v)", bin, err)
			}
		}
	}

	env.legs = genLegs

	return env
}

// genAllowlist is the checked-in allowlist as it applies to a generated
// program.
func genAllowlist(tb testing.TB) divergence.Allowlist {
	tb.Helper()

	return mustAllowlist(tb).RulesEverywhere()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()

	v := os.Getenv(key)
	if v == "" {
		return def
	}

	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		t.Fatalf("%s=%q: want a non-negative integer", key, v)
	}

	return n
}

// rootPath resolves a path given relative to the repo root, where make runs.
func rootPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}

	return filepath.Join(root, p)
}

// genSeeds is the seeds to run and whether they came from GEN_SEEDS rather
// than the smoke set.
func genSeeds(t *testing.T) ([]uint64, bool) {
	t.Helper()

	spec, count := os.Getenv("GEN_SEEDS"), os.Getenv("GEN_COUNT")

	switch {
	case spec == "" && count != "":
		t.Fatal("GEN_COUNT needs GEN_SEEDS")
	case spec == "":
		return genSmokeSeeds, false
	case strings.Contains(spec, ","):
		var seeds []uint64

		for s := range strings.SplitSeq(spec, ",") {
			seeds = append(seeds, parseSeed(t, s))
		}

		return seeds, true
	}

	n := envInt(t, "GEN_COUNT", 20)

	var start uint64

	if spec == "random" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}

		start = binary.LittleEndian.Uint64(b[:]) >> 16
		t.Logf("GEN_SEEDS=random drew %d (rerun: GEN_SEEDS=%d GEN_COUNT=%d)", start, start, n)
	} else {
		start = parseSeed(t, spec)
	}

	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = start + uint64(i)
	}

	return seeds, true
}

func parseSeed(t *testing.T, s string) uint64 {
	t.Helper()

	seed, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		t.Fatalf("GEN_SEEDS: %q is not a seed: %v", s, err)
	}

	return seed
}

// evalGenProgram runs prog under mrp and under every leg, in a scratch
// directory it removes once done: a shrink runs many programs per test. It
// never fails the test, so the shrinker may call it from several goroutines.
func evalGenProgram(t *testing.T, env genEnv, prog *gen.Program) (genVerdict, error) {
	t.Helper()

	work := t.TempDir()

	defer func() { _ = os.RemoveAll(work) }()

	dir := filepath.Join(work, "fixture")
	if err := prog.WriteFixture(dir); err != nil {
		return genVerdict{}, err
	}

	return evalGenInvocations(t, env, dir, work)
}

// evalGenInvocations runs the fixture at dir at its own invocation and,
// where it differs, at its witness invocation, which reaches the shapes
// non-empty collections fork into.
func evalGenInvocations(t *testing.T, env genEnv, dir, work string) (genVerdict, error) {
	t.Helper()

	literal, err := evalGenFixture(t, env, dir, work)
	if err != nil {
		return genVerdict{}, err
	}

	text, differs, err := witnessSource(dir)

	switch {
	case err != nil && literal.skip != "":
		return literal, nil
	case err != nil:
		return genVerdict{}, err
	case !differs:
		return literal, nil
	}

	wwork := filepath.Join(work, "witness")
	wdir := filepath.Join(wwork, "fixture")

	if err := writeWitnessFixture(dir, wdir, text); err != nil {
		return genVerdict{}, err
	}

	witness, err := evalGenFixture(t, env, wdir, wwork)
	if err != nil {
		return genVerdict{}, fmt.Errorf("witness invocation: %w", err)
	}

	return joinVerdicts(literal, witness), nil
}

// evalGenFixture runs the fixture at dir under mrp and under every leg, in
// the scratch directory work.
func evalGenFixture(t *testing.T, env genEnv, dir, work string) (genVerdict, error) {
	t.Helper()

	transpileErrs, skip := transpileGenLegs(env, dir, work)
	if skip != "" {
		return genVerdict{skip: skip}, nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), env.mrpTimeout)
	defer cancel()

	mrp, err := mrpOutcome(ctx, env.martian, dir, "pipeline.mro", work)

	switch {
	case errors.Is(err, errMrpUnfinished) && t.Context().Err() == nil:
		return genVerdict{skip: fmt.Sprintf("mrp did not finish in %s:\n%s", env.mrpTimeout,
			tailFile(filepath.Join(work, "mrp.log")))}, nil
	case mrpFailedOutsideStages(err):
		return genVerdict{skip: fmt.Sprintf("mrp failed outside any stage (%v):\n%s", err,
			tailFile(filepath.Join(work, "mrp.log")))}, nil
	case err != nil:
		return genVerdict{}, err
	}

	if unresolved, err := mrpRanUnresolved(filepath.Join(work, "mrp.log")); err != nil || unresolved {
		return genVerdict{skip: "mrp ran a stage whose bindings it failed to resolve (#606):\n" + tailFile(filepath.Join(work, "mrp.log"))}, err
	}

	if mrp.Failed && !slices.ContainsFunc(mrp.Failures, func(f stageFailure) bool { return isStageDir(dir, f.Stage) }) {
		return genVerdict{skip: fmt.Sprintf("mrp failed in no stage (%s), in its own checks or fork resolution:\n%s",
			mrp, tailFile(filepath.Join(work, "mrp.log")))}, nil
	}

	var v genVerdict

	for i, leg := range env.legs {
		lv, err := runGenLeg(t, env, leg, dir, work, mrp, transpileErrs[i])
		if err != nil {
			return genVerdict{}, fmt.Errorf("leg %s: %w", leg.name, err)
		}

		if lv.skip != "" {
			return lv, nil
		}

		v.findings = append(v.findings, lv.findings...)
		v.notes = append(v.notes, lv.notes...)
	}

	return v, nil
}

// transpileGenLegs transpiles the fixture at dir for every leg into its
// genLegProj before anything runs, so a program one leg rejects as
// unsupported is no test before any leg runs, never after another leg found
// a divergence in it. It returns each leg's transpile error, or why the
// program is no test.
func transpileGenLegs(env genEnv, dir, work string) ([]error, string) {
	errs := make([]error, len(env.legs))

	for i, leg := range env.legs {
		err := transpileInto(env.tools, dir, "pipeline.mro", genLegProj(work, leg))
		if err != nil && (strings.Contains(err.Error(), cg.ErrShapeUnsupported.Error()) || strings.Contains(err.Error(), keywordInputRefusal)) {
			return nil, fmt.Sprintf("mro2nf rejects it as unsupported (%s leg): %v", leg.name, err)
		}

		errs[i] = err
	}

	return errs, ""
}

func genLegProj(work string, leg genLeg) string { return filepath.Join(work, "nf-"+leg.name) }

// planRefusal is the error with which the failed run of proj's PLAN
// refused the call graph as a shape mro2nf does not wire, which it can tell
// only from the launch values, as transpileGenLegs's refusals are told from
// the source; "" when PLAN refused nothing.
func planRefusal(proj string) (string, error) {
	rows, err := parseTrace(filepath.Join(proj, "trace.txt"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	for _, r := range rows {
		if r.Name != "PLAN" || r.Status != "FAILED" {
			continue
		}

		errs, err := filepath.Glob(filepath.Join(proj, "work", r.Hash+"*", ".command.err"))
		if err != nil {
			return "", fmt.Errorf("glob PLAN stderr: %w", err)
		}

		for _, path := range errs {
			raw, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("read PLAN stderr: %w", err)
			}

			if strings.Contains(string(raw), cg.ErrShapeUnsupported.Error()) {
				return strings.TrimSpace(string(raw)), nil
			}
		}
	}

	return "", nil
}

// isStageDir reports whether name is a stage of the fixture at dir, rather
// than a pipeline: mrp records its own failures resolving a pipeline's
// forks or merging its outputs at the pipeline, and sometimes only by chance
// of timing.
func isStageDir(dir, name string) bool {
	return name != "" && fileExists(filepath.Join(dir, filepath.FromSlash(gen.StageDir(name))))
}

// runGenLeg runs the leg's transpiled project, which transpileErr reports
// failed to transpile, and diffs the run against mrp's: its outcome when
// either failed, else its outputs.
func runGenLeg(t *testing.T, env genEnv, leg genLeg, dir, work string, mrp runOutcome, transpileErr error) (genVerdict, error) {
	t.Helper()

	if transpileErr != nil {
		return outcomeVerdict(leg.name, mrp, failedWith(nil), transpileErr.Error()), nil
	}

	proj := genLegProj(work, leg)
	fixture := filepath.Base(dir)
	runErr := runNextflow(t, proj, append(mrpPoolArgs(t, proj), "-with-trace", "trace.txt")...)
	recovered := errors.Is(runErr, errFailedAttempt)

	if runErr != nil && !recovered {
		refusal, err := planRefusal(proj)
		if err != nil || refusal != "" {
			return genVerdict{skip: fmt.Sprintf("mro2nf's PLAN rejects it as unsupported (%s leg): %s", leg.name, refusal)}, err
		}
	}

	if (runErr != nil && !recovered) || mrp.Failed {
		failure := runErr
		if recovered {
			failure = nil
		}

		return outcomeLegVerdict(env, leg, fixture, proj, mrp, failure)
	}

	v, err := outputsVerdict(env, leg, fixture, work, proj)
	if err != nil {
		return genVerdict{}, err
	}

	tasks, err := stageTasksVerdict(leg.name, filepath.Join(work, "mrp"), proj)
	if err != nil {
		return genVerdict{}, err
	}

	v.findings = append(v.findings, tasks.findings...)
	v.notes = append(v.notes, tasks.notes...)

	if recovered {
		v.findings = append(v.findings, genFinding{leg: leg.name, key: "attempts", detail: runErr.Error()})
	}

	return v, nil
}

// stageTasksVerdict holds the leg's stage tasks in proj to the jobs mrp ran
// in the pipestance ps, one task per job (the overhead rule), and notes
// both counts.
func stageTasksVerdict(leg, ps, proj string) (genVerdict, error) {
	jobs, err := mrpJobCount(ps)
	if err != nil {
		return genVerdict{}, err
	}

	tasks, err := stageTaskCount(proj)
	if err != nil {
		return genVerdict{}, err
	}

	counts := fmt.Sprintf("%d stage tasks, mrp ran %d jobs", tasks, jobs)
	v := genVerdict{notes: []string{leg + ": " + counts}}

	if tasks != jobs {
		v.findings = append(v.findings, genFinding{leg: leg, key: "tasks", detail: counts})
	}

	return v, nil
}

// outcomeLegVerdict compares the outcome of the leg's Nextflow run of proj,
// which failed with runErr or succeeded when it is nil, with mrp's, and
// reports each outcome rule the run needs that env's allowlist does not
// excuse for the program fixture, as TestMrpDiffFailureParity does.
func outcomeLegVerdict(env genEnv, leg genLeg, fixture, proj string, mrp runOutcome, runErr error) (genVerdict, error) {
	nf, attempts, err := nextflowOutcome(proj, runErr)
	if err != nil {
		return genVerdict{}, err
	}

	stamped, err := stageStamped(env, proj)
	if err != nil {
		return genVerdict{}, err
	}

	// Without stamps a failure cannot be attributed to its stage, so only
	// whether the run failed, and how, can be compared.
	if !stamped {
		mrp, nf = anonymous(mrp), anonymous(nf)
	}

	detail := "the Nextflow run succeeded"
	if runErr != nil {
		detail = runErr.Error()
	}

	v := outcomeVerdict(leg.name, mrp, nf, detail)
	v.notes = append(v.notes, fmt.Sprintf("%s: mrp's run: %s; mro2nf's: %s", leg.name, mrp, nf))

	if reports := outcomeRuleReports(env.allow, fixture, attempts); len(reports) > 0 {
		v.findings = append(v.findings, genFinding{leg: leg.name, key: "outcome-rules", detail: strings.Join(reports, "\n")})
	}

	return v, nil
}

// outcomeVerdict compares two run outcomes, as TestMrpDiffFailureParity does.
func outcomeVerdict(leg string, mrp, nf runOutcome, detail string) genVerdict {
	if mrp.equal(nf) {
		return genVerdict{}
	}

	return genVerdict{findings: []genFinding{{
		leg: leg, key: "outcome",
		detail: fmt.Sprintf("mrp: %s; mro2nf: %s\n%s", mrp, nf, detail),
	}}}
}

// outputsVerdict diffs two successful runs of the program fixture, mrp's in
// work and the leg's in proj: their published outputs and, where the run left
// stage metadata, their stage ABI.
func outputsVerdict(env genEnv, leg genLeg, fixture, work, proj string) (genVerdict, error) {
	ps := filepath.Join(work, "mrp")

	findings, err := genPublishedFindings(env.allow, leg.name, fixture, ps, filepath.Join(proj, "results"))
	if err != nil {
		return genVerdict{}, err
	}

	v := genVerdict{findings: findings}

	stamped, err := stageStamped(env, proj)
	if err != nil {
		return genVerdict{}, err
	}

	if !stamped {
		v.notes = append(v.notes, "the prebuilt mre wrote no "+mreStageFile+" stamps (it predates #363): stage ABI not compared")

		return v, nil
	}

	abi, err := stageABIGroups(abiRulesFor(env.allow, fixture), ps, proj)
	if err != nil {
		return genVerdict{}, err
	}

	for _, group := range slices.Sorted(maps.Keys(abi)) {
		v.findings = append(v.findings, genFinding{leg: leg.name, key: "abi:" + group, detail: abi[group]})
	}

	return v, nil
}

// stageStamped reports whether the run of proj recorded which stage each
// task ran, which the stage ABI and failure attribution read: a prebuilt mre
// older than the stamps (#363) does not.
func stageStamped(env genEnv, proj string) (bool, error) {
	if !env.prebuilt {
		return true, nil
	}

	stamps, err := filepath.Glob(filepath.Join(proj, "work", "*", "*", "*", mreStageFile))
	if err != nil {
		return false, fmt.Errorf("glob stage stamps: %w", err)
	}

	return len(stamps) > 0, nil
}

// anonymous is o with the stages its failures blame dropped.
func anonymous(o runOutcome) runOutcome {
	failures := make([]stageFailure, len(o.Failures))
	for i, f := range o.Failures {
		failures[i] = stageFailure{Kind: f.Kind}
	}

	if !o.Failed {
		return o
	}

	return failedWith(failures)
}

// genPublishedFindings reports each top-level output whose published files
// or _outs value differ between the mrp pipestance ps of the program fixture
// and the leg's results dir, less what allow excuses.
func genPublishedFindings(allow divergence.Allowlist, leg, fixture, ps, results string) ([]genFinding, error) {
	pub, err := loadPublished(allow, fixture, ps, results)
	if err != nil {
		return nil, err
	}

	outs, err := outsFindings(leg, pub.mrpOuts, pub.nfOuts)
	if err != nil {
		return nil, err
	}

	return append(treeFindings(leg, pub), outs...), nil
}

// treeFindings reports each top-level output whose published files differ
// between the runs.
func treeFindings(leg string, pub published) []genFinding {
	byTop := map[string][]string{}

	for _, rel := range slices.Sorted(maps.Keys(union(pub.mrpTree, pub.nfTree))) {
		a, inMrp := pub.mrpTree[rel]
		b, inNF := pub.nfTree[rel]

		if a != b || inMrp != inNF {
			top, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			byTop[top] = append(byTop[top], rel)
		}
	}

	out := make([]genFinding, 0, len(byTop))

	for _, top := range slices.Sorted(maps.Keys(byTop)) {
		pick := func(tree map[string]string) map[string]string {
			sub := map[string]string{}

			for _, rel := range byTop[top] {
				if h, ok := tree[rel]; ok {
					sub[rel] = h
				}
			}

			return sub
		}

		out = append(out, genFinding{
			leg: leg, key: "tree:" + top,
			detail: "published files differ (-mrp +nextflow):\n" + cmp.Diff(pick(pub.mrpTree), pick(pub.nfTree)),
		})
	}

	return out
}

// outsFindings reports each top-level key of the _outs JSON whose value
// differs between the runs, else the order of the keys when only it differs,
// or the whole document when either is no object.
func outsFindings(leg string, mrpOuts, nfOuts any) ([]genFinding, error) {
	m, mOK := mrpOuts.(jsonObject)
	n, nOK := nfOuts.(jsonObject)

	if !mOK || !nOK {
		return outsDocFinding(leg, "outs", mrpOuts, nfOuts)
	}

	mv, err := encodedMembers(m)
	if err != nil {
		return nil, err
	}

	nv, err := encodedMembers(n)
	if err != nil {
		return nil, err
	}

	var out []genFinding

	for _, k := range slices.Sorted(maps.Keys(union(mv, nv))) {
		a, inMrp := mv[k]
		b, inNF := nv[k]

		if a != b || inMrp != inNF {
			out = append(out, genFinding{
				leg: leg, key: "outs:" + k,
				detail: fmt.Sprintf("_outs value differs (in mrp %t, in nextflow %t; -mrp +nextflow):\n%s",
					inMrp, inNF, cmp.Diff(indentJSON(a), indentJSON(b))),
			})
		}
	}

	if len(out) > 0 {
		return out, nil
	}

	return outsDocFinding(leg, "outs-order", mrpOuts, nfOuts)
}

// outsDocFinding reports the whole _outs JSON under key when it differs
// between the runs.
func outsDocFinding(leg, key string, mrpOuts, nfOuts any) ([]genFinding, error) {
	a, err := encodeOrderedJSON(mrpOuts)
	if err != nil {
		return nil, err
	}

	b, err := encodeOrderedJSON(nfOuts)
	if err != nil {
		return nil, err
	}

	if a == b {
		return nil, nil
	}

	return []genFinding{{leg: leg, key: key, detail: "(-mrp +nextflow):\n" + cmp.Diff(indentJSON(a), indentJSON(b))}}, nil
}

// encodedMembers maps each member key of obj to its value's compact JSON.
func encodedMembers(obj jsonObject) (map[string]string, error) {
	out := make(map[string]string, len(obj))

	for _, m := range obj {
		v, err := encodeOrderedJSON(m.Value)
		if err != nil {
			return nil, err
		}

		out[m.Key] = v
	}

	return out, nil
}

func union[V any](a, b map[string]V) map[string]bool {
	keys := map[string]bool{}

	for k := range a {
		keys[k] = true
	}

	for k := range b {
		keys[k] = true
	}

	return keys
}

// shrinkGenProgram shrinks prog while a finding of v persists, and returns
// the smallest program and its verdict.
func shrinkGenProgram(t *testing.T, env genEnv, prog *gen.Program, v genVerdict) (*gen.Program, genVerdict) {
	t.Helper()

	want := shrinkTarget(v, env.keep)

	var mu sync.Mutex

	verdicts := map[string]genVerdict{prog.Key(): v}
	fails := func(p *gen.Program) bool {
		cv, err := evalGenProgram(t, env, p)
		if err != nil {
			t.Logf("shrink candidate: %v", err)

			return false
		}

		if cv.skip != "" || !cv.sharesFinding(want) {
			return false
		}

		mu.Lock()
		verdicts[p.Key()] = cv
		mu.Unlock()

		return true
	}

	small, evals := gen.Shrink(prog, fails, env.shrink)
	t.Logf("shrank seed %d in %d runs: %s -> %s", prog.Seed, evals, gen.Size(prog), gen.Size(small))

	return small, verdicts[small.Key()]
}

// writeGenFixture writes p under env.artifacts as gen_<hash>/ in the testdata
// layout, with mrp's golden (expected/outs.json and the published tree in
// expected/outs/) when mrp succeeds and the divergence report beside it.
func writeGenFixture(t *testing.T, env genEnv, p *gen.Program, v genVerdict) (string, error) {
	t.Helper()

	sum := sha256.Sum256([]byte(p.Key()))
	dir := filepath.Join(env.artifacts, "gen_"+hex.EncodeToString(sum[:])[:12])

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear %s: %w", dir, err)
	}

	if err := p.WriteFixture(dir); err != nil {
		return "", err
	}

	if v.atWitness() {
		text, _, err := witnessSource(dir)
		if err != nil {
			return "", err
		}

		if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(text), 0o600); err != nil {
			return "", fmt.Errorf("write witness pipeline.mro: %w", err)
		}
	}

	tmp := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), env.mrpTimeout)
	defer cancel()

	mrp, err := mrpOutcome(ctx, env.martian, dir, "pipeline.mro", tmp)
	if err != nil {
		return "", err
	}

	if !mrp.Failed {
		if err := recordGenGolden(env.allow, tmp, dir); err != nil {
			return "", err
		}
	}

	legs := make([]string, len(env.legs))
	for i, l := range env.legs {
		legs[i] = l.name
	}

	report := fmt.Sprintf("seed %d (attempt %d)\nmro2nf %s\nmre %s\nlegs %s\nmrp: %s\n\n%s",
		p.Seed, p.Attempt, env.tools.mro2nf, env.tools.mre, strings.Join(legs, ","), mrp, v)
	if err := os.WriteFile(filepath.Join(dir, "divergence.txt"), []byte(report), 0o644); err != nil {
		return "", fmt.Errorf("write divergence report: %w", err)
	}

	return dir, removePycache(dir)
}

// recordGenGolden records the mrp pipestance at <tmp>/mrp as the golden of
// the program fixture at dir: expected/outs.json, its outs JSON as the
// published-output comparison reads it under allow, and the published tree
// TestRecordGoldens would record.
func recordGenGolden(allow divergence.Allowlist, tmp, dir string) error {
	ps, fixture := filepath.Join(tmp, "mrp"), filepath.Base(dir)

	outs, err := mrpOutsJSON(allow, fixture, ps, nil)
	if err != nil {
		return err
	}

	compact, err := encodeOrderedJSON(outs)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(compact), "", "    "); err != nil {
		return fmt.Errorf("indent golden outs: %w", err)
	}

	buf.WriteByte('\n')

	if err := os.MkdirAll(filepath.Join(dir, "expected"), 0o755); err != nil {
		return fmt.Errorf("create expected dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "expected", "outs.json"), buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write golden outs: %w", err)
	}

	return recordTree(filepath.Join(ps, "outs"), dir, allow.Excuses(divergence.RuleMrpPublishedSymlinks, fixture))
}

// removePycache deletes the bytecode caches the golden mrp run left in the
// fixture's stage directories.
func removePycache(dir string) error {
	caches, err := filepath.Glob(filepath.Join(dir, "stages", "*", "__pycache__"))
	if err != nil {
		return fmt.Errorf("glob bytecode caches: %w", err)
	}

	for _, c := range caches {
		if err := os.RemoveAll(c); err != nil {
			return fmt.Errorf("remove %s: %w", c, err)
		}
	}

	return nil
}
