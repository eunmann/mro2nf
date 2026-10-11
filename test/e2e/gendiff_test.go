//go:build e2e

package e2e

// The generative differential (#350): random well-typed Martian programs from
// test/gen run under real mrp and under mro2nf + Nextflow, and face the same
// checks TestMrpDiff and TestMrpDiffFailureParity apply to the fixtures: the
// published tree, the _outs JSON and every stage invocation's _args/_outs
// when both runs succeed, the run outcome when either fails. A divergence is
// shrunk to a minimal program and written as a fixture directory.
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
//	GEN_LEGS           comma list of genLegs names (default genDefaultLegs).
//	GEN_MRO2NF_BIN,    prebuilt binaries to test instead of the repo build,
//	GEN_MRE_BIN        e.g. an older commit's.
//	GEN_ARTIFACTS      where shrunk fixtures go (default artifacts/gen-diff).
//	GEN_SHRINK_EVALS   shrink budget in program runs (default 150; 0 skips).
//	GEN_SHRINK_PARALLEL candidates run at once while shrinking (default 3).
//	GEN_SHRINK_KEEP    regexp of finding ids ("<leg> <key>") the shrink must
//	                   keep, to isolate one of several divergences.
//	GEN_FIXTURES       comma list of fixture dirs (such as shrunk ones) to
//	                   rerun instead of generating programs.
//	GEN_MRP_TIMEOUT    seconds an mrp run may take (default 180).

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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eunmann/mro2nf/test/divergence"
	"github.com/eunmann/mro2nf/test/gen"
	"github.com/google/go-cmp/cmp"
)

// genSmokeSeeds are the seeds every PR runs: between them their programs
// reach every construct and corpus value test/gen covers (gen.Features) at
// least once, mrp runs each of them, and mro2nf matches mrp on all four legs.
var genSmokeSeeds = []uint64{1, 2, 3, 4, 6, 8, 9, 10, 11, 12, 13, 14, 15, 22, 43, 108, 154, 192, 193, 246}

// genDefaultLegs run every program under each transpile mode: what a sub
// pipeline does at a call site differs between inlined and kept boundaries.
const genDefaultLegs = "default,noinline,native,native-runner"

// genLegs are the transpile modes a program can run under, as TestMrpDiff's
// legs. The native runner calls stage functions in-process and records no
// stage metadata, so its leg has no stage ABI to compare and no stage to
// blame a failure on.
var genLegs = map[string]genLeg{
	"default":       {name: "default", stamped: true},
	"noinline":      {name: "noinline", flags: []string{"-inline-pipelines=false"}, stamped: true},
	"native":        {name: "native", flags: []string{"-native"}, stamped: true},
	"native-runner": {name: "native-runner", flags: []string{"-native", "-native-runner"}},
}

type genLeg struct {
	name    string
	flags   []string
	stamped bool
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
}

// TestGenDiff runs each selected seed's program under mrp and mro2nf and
// fails on any divergence, naming the seed and the shrunk fixture.
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
			runGenSeed(t, env, seed, random)
		})
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

	t.Errorf("seed %d diverges from mrp (rerun: GEN_SEEDS=%d GEN_COUNT=1 make gen-diff):\n%s", seed, seed, v)

	small, sv := shrinkGenProgram(t, env, prog, v)

	dir, err := writeGenFixture(t, env, small, sv)
	if err != nil {
		t.Fatal(err)
	}

	t.Errorf("seed %d: shrunk fixture %s (%s):\n%s", seed, dir, gen.Size(small), sv)
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

func loadGenEnv(t *testing.T) genEnv {
	t.Helper()

	env := genEnv{
		martian:   martianBin(t),
		tools:     repoTools(),
		allow:     genAllowlist(t),
		artifacts: rootPath(envOr("GEN_ARTIFACTS", filepath.Join("artifacts", "gen-diff"))),
		shrink: gen.ShrinkOptions{
			MaxEvals: envInt(t, "GEN_SHRINK_EVALS", 150),
			Parallel: envInt(t, "GEN_SHRINK_PARALLEL", 3),
		},
		mrpTimeout: time.Duration(envInt(t, "GEN_MRP_TIMEOUT", 180)) * time.Second,
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

	for name := range strings.SplitSeq(envOr("GEN_LEGS", genDefaultLegs), ",") {
		leg, ok := genLegs[name]
		if !ok {
			t.Fatalf("GEN_LEGS: unknown leg %q (have %v)", name, slices.Sorted(maps.Keys(genLegs)))
		}

		env.legs = append(env.legs, leg)
	}

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

// genFinding is one way a run diverged from mrp, under one leg. Its key names
// what diverged (the outcome, a published output, a stage phase's ABI) so a
// shrink can require the same divergence to persist.
type genFinding struct {
	leg    string
	key    string
	detail string
}

func (f genFinding) id() string { return f.leg + " " + f.key }

// genVerdict is what running one program found. A program with a skip reason
// is no test: mrp rejects it before any stage runs, or mro2nf rejects it as a
// documented unsupported construct.
type genVerdict struct {
	skip     string
	findings []genFinding
	notes    []string
}

func (v genVerdict) String() string {
	var b strings.Builder

	for _, f := range v.findings {
		fmt.Fprintf(&b, "  [%s] %s\n    %s\n", f.leg, f.key, strings.ReplaceAll(strings.TrimRight(f.detail, "\n"), "\n", "\n    "))
	}

	return b.String()
}

func (v genVerdict) sharesFinding(ids map[string]bool) bool {
	return slices.ContainsFunc(v.findings, func(f genFinding) bool { return ids[f.id()] })
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

	return evalGenFixture(t, env, dir, work)
}

// evalGenFixture runs the fixture at dir under mrp and under every leg, in
// the scratch directory work.
func evalGenFixture(t *testing.T, env genEnv, dir, work string) (genVerdict, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), env.mrpTimeout)
	defer cancel()

	mrp, err := mrpOutcome(ctx, env.martian, dir, work)

	switch {
	case errors.Is(err, errMrpUnfinished) && t.Context().Err() == nil:
		return genVerdict{skip: fmt.Sprintf("mrp did not finish in %s:\n%s", env.mrpTimeout,
			tailFile(filepath.Join(work, "mrp.log")))}, nil
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, errABIMeta):
		// mrp failed a fork it never invoked as a call, in its own fork
		// resolution ("matched 2 out of 2 forks ..., need exactly 1"): it
		// rejects the program at run time.
		return genVerdict{skip: fmt.Sprintf("mrp failed outside any stage (%v):\n%s", err,
			tailFile(filepath.Join(work, "mrp.log")))}, nil
	case err != nil:
		return genVerdict{}, err
	}

	if mrp.Failed && !slices.ContainsFunc(mrp.Failures, func(f stageFailure) bool { return isStageDir(dir, f.Stage) }) {
		return genVerdict{skip: fmt.Sprintf("mrp failed in no stage (%s), in its own checks or fork resolution:\n%s",
			mrp, tailFile(filepath.Join(work, "mrp.log")))}, nil
	}

	var v genVerdict

	for _, leg := range env.legs {
		lv, err := runGenLeg(t, env, leg, dir, work, mrp)
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

// isStageDir reports whether name is a stage of the fixture at dir, rather
// than a pipeline: mrp records its own failures resolving a pipeline's
// forks or merging its outputs at the pipeline, and sometimes only by chance
// of timing.
func isStageDir(dir, name string) bool {
	return name != "" && fileExists(filepath.Join(dir, filepath.FromSlash(gen.StageDir(name))))
}

// runGenLeg transpiles and runs the fixture under one leg and diffs the run
// against mrp's: its outcome when either failed, else its outputs.
func runGenLeg(t *testing.T, env genEnv, leg genLeg, dir, work string, mrp runOutcome) (genVerdict, error) {
	t.Helper()

	proj := filepath.Join(work, "nf-"+leg.name)

	if err := transpileInto(env.tools, dir, "pipeline.mro", proj, leg.flags...); err != nil {
		if strings.Contains(err.Error(), "unsupported construct") {
			return genVerdict{skip: fmt.Sprintf("mro2nf rejects it as unsupported (%s leg): %v", leg.name, err)}, nil
		}

		return outcomeVerdict(leg.name, mrp, failedWith(nil), err.Error()), nil
	}

	fixture := filepath.Base(dir)
	runErr := runNextflow(t, proj, "-with-trace", "trace.txt")
	recovered := errors.Is(runErr, errFailedAttempt)

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

	if recovered {
		v.findings = append(v.findings, genFinding{leg: leg.name, key: "attempts", detail: runErr.Error()})
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

	stamped, err := stageStamped(env, leg, proj)
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

	stamped, err := stageStamped(env, leg, proj)
	if err != nil {
		return genVerdict{}, err
	}

	if !stamped {
		if leg.stamped {
			v.notes = append(v.notes, "the prebuilt mre wrote no "+mreStageFile+" stamps (it predates #363): stage ABI not compared")
		}

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
// task ran, which the stage ABI and failure attribution read: the native
// runner's leg never does, and neither does a prebuilt mre older than the
// stamps (#363).
func stageStamped(env genEnv, leg genLeg, proj string) (bool, error) {
	if !leg.stamped {
		return false, nil
	}

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

	verdicts := map[string]genVerdict{progKey(prog): v}
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
		verdicts[progKey(p)] = cv
		mu.Unlock()

		return true
	}

	small, evals := gen.Shrink(prog, fails, env.shrink)
	t.Logf("shrank seed %d in %d runs: %s -> %s", prog.Seed, evals, gen.Size(prog), gen.Size(small))

	return small, verdicts[progKey(small)]
}

// shrinkTarget is the findings a shrink keeps one of: those keep matches,
// when it matches any; else a different outcome when there is one, else stage
// ABI differences, which locate where a divergence starts, else any. Keeping
// the most upstream evidence stops the shrink from trading the divergence for
// a downstream one it caused.
func shrinkTarget(v genVerdict, keep *regexp.Regexp) map[string]bool {
	if keep != nil {
		want := map[string]bool{}

		for _, f := range v.findings {
			if keep.MatchString(f.id()) {
				want[f.id()] = true
			}
		}

		if len(want) > 0 {
			return want
		}
	}

	for _, class := range []string{"outcome", "abi:", ""} {
		want := map[string]bool{}

		for _, f := range v.findings {
			if strings.HasPrefix(f.key, class) {
				want[f.id()] = true
			}
		}

		if len(want) > 0 {
			return want
		}
	}

	return nil
}

// progKey identifies a program by its source, whatever its shrunk mark.
func progKey(p *gen.Program) string {
	c := *p
	c.Shrunk = false

	return c.MRO()
}

// writeGenFixture writes p under env.artifacts as gen_<hash>/ in the testdata
// layout, with mrp's golden (expected/outs.json and the published tree in
// expected/outs/) when mrp succeeds and the divergence report beside it.
func writeGenFixture(t *testing.T, env genEnv, p *gen.Program, v genVerdict) (string, error) {
	t.Helper()

	sum := sha256.Sum256([]byte(progKey(p)))
	dir := filepath.Join(env.artifacts, "gen_"+hex.EncodeToString(sum[:])[:12])

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear %s: %w", dir, err)
	}

	if err := p.WriteFixture(dir); err != nil {
		return "", err
	}

	tmp := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), env.mrpTimeout)
	defer cancel()

	mrp, err := mrpOutcome(ctx, env.martian, dir, tmp)
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

	outs, err := mrpOutsJSON(allow, fixture, ps)
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
