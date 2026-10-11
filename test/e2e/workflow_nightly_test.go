//go:build e2e

package e2e

import (
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

const (
	nightlyWorkflow = "nightly.yml"

	// nightlyJobPrefix starts every nightly job name. No required context
	// starts with it, so no matrix expansion of a nightly name can post one.
	nightlyJobPrefix = "Nightly"

	// nightlyFailureGate keeps a dispatch from another branch, which may be
	// trying out its own broken nightly, from filing the main-branch issue.
	nightlyFailureGate = "${{ failure() && github.ref == 'refs/heads/main' }}"
)

// TestNightlyRunsOnSchedule pins the nightly triggers: a cron schedule plus a
// manual dispatch, and no push or PR trigger, whose failures would open the
// nightly failure issue for a branch's own breakage.
func TestNightlyRunsOnSchedule(t *testing.T) {
	on := readWorkflow(t, nightlyWorkflow).get("on")

	sched := on.get("schedule")
	if sched == nil || len(sched.items) == 0 {
		t.Fatalf("%s has no schedule trigger", nightlyWorkflow)
	}

	for _, s := range sched.items {
		if c := s.get("cron"); c == nil || c.scalar == "" {
			t.Errorf("%s schedule entry has no cron", nightlyWorkflow)
		}
	}

	if on.get("workflow_dispatch") == nil {
		t.Errorf("%s has no workflow_dispatch trigger", nightlyWorkflow)
	}

	for _, ev := range []string{"push", "pull_request", "pull_request_target"} {
		if on.get(ev) != nil {
			t.Errorf("%s triggers on %s", nightlyWorkflow, ev)
		}
	}
}

// TestNightlyNoRequiredContexts holds every nightly job name to the
// "Nightly" prefix. TestRequiredContextsOnlyFromPRValidation compares names
// as written, so a `${{ matrix.* }}` name could still expand to a required
// context and satisfy branch protection; a prefixed name cannot.
func TestNightlyNoRequiredContexts(t *testing.T) {
	for _, c := range requiredContexts {
		if strings.HasPrefix(c, nightlyJobPrefix) {
			t.Fatalf("required context %q starts with %q; the prefix no longer separates the tiers", c, nightlyJobPrefix)
		}
	}

	for _, j := range workflowJobs(t, nightlyWorkflow) {
		if !strings.HasPrefix(j.name, nightlyJobPrefix+" ") {
			t.Errorf("%s job %q is named %q, want a %q prefix", nightlyWorkflow, j.id, j.name, nightlyJobPrefix+" ")
		}
	}
}

// TestNightlyFailureReport pins the job that opens or updates the nightly
// failure issue: it runs only on a main-branch failure, after every other
// job, and holds the issues permission its gh calls need.
func TestNightlyFailureReport(t *testing.T) {
	jobs := workflowJobs(t, nightlyWorkflow)

	var (
		reporters []workflowJob
		others    []string
	)

	for _, j := range jobs {
		switch {
		case j.cond == nightlyFailureGate:
			reporters = append(reporters, j)
		case !slices.Contains(nightlyDiscoveryJobs, j.id):
			others = append(others, j.id)
		}
	}

	if len(reporters) != 1 {
		t.Fatalf("%s has %d jobs with if %q, want 1", nightlyWorkflow, len(reporters), nightlyFailureGate)
	}

	r := reporters[0]

	needs := slices.Sorted(slices.Values(r.needs))
	slices.Sort(others)

	if diff := cmp.Diff(others, needs); diff != "" {
		t.Errorf("report job %q must need every job but the discovery jobs (-want +got):\n%s", r.id, diff)
	}

	job := readWorkflow(t, nightlyWorkflow).get("jobs").get(r.id)
	if got := job.get("permissions").get("issues"); got == nil || got.scalar != "write" {
		t.Errorf("report job %q lacks permissions issues: write", r.id)
	}

	var script strings.Builder
	for _, step := range job.get("steps").items {
		if run := step.get("run"); run != nil {
			script.WriteString(run.scalar)
		}
	}

	for _, want := range []string{"gh issue create", "gh issue comment"} {
		if !strings.Contains(script.String(), want) {
			t.Errorf("report job %q never runs %q", r.id, want)
		}
	}
}

var manifestFloor = regexp.MustCompile(`(?m)^manifest\.nextflowVersion = '!>=([0-9.]+)'$`)

// nightlySuiteJobs are the nightly jobs that run an e2e suite on the Nextflow
// matrix.
var nightlySuiteJobs = []string{"e2e", "docker"}

// TestNightlyNextflowFloor requires every nightly suite matrix to run the
// Nextflow floor the generated nextflow.config declares, and latest stable,
// so a floor bump in the emitter moves the CI floor with it.
func TestNightlyNextflowFloor(t *testing.T) {
	proj := transpile(t, "file_min")

	cfg, err := os.ReadFile(filepath.Join(proj, "nextflow.config"))
	if err != nil {
		t.Fatalf("read nextflow.config: %v", err)
	}

	m := manifestFloor.FindSubmatch(cfg)
	if m == nil {
		t.Fatalf("generated nextflow.config declares no manifest.nextflowVersion floor:\n%s", cfg)
	}

	jobs := readWorkflow(t, nightlyWorkflow).get("jobs")

	for _, id := range nightlySuiteJobs {
		got := jobs.get(id).get("strategy").get("matrix").get("nextflow").list()

		for _, want := range []string{string(m[1]), "latest-stable"} {
			if !slices.Contains(got, want) {
				t.Errorf("%s %s matrix nextflow %q lacks %q", nightlyWorkflow, id, got, want)
			}
		}
	}
}

// TestNightlyShardsPartitionSuite holds the nightly e2e shards to harness
// shardSkip's contract: E2E_SHARD_TOTAL is the shard count and the shards are
// 0..N-1, so their union runs every test exactly once.
func TestNightlyShardsPartitionSuite(t *testing.T) {
	job := readWorkflow(t, nightlyWorkflow).get("jobs").get("e2e")

	shards := job.get("strategy").get("matrix").get("shard").list()
	if len(shards) < 2 {
		t.Fatalf("%s e2e matrix shard %q: want at least 2 shards", nightlyWorkflow, shards)
	}

	for i, s := range shards {
		if s != strconv.Itoa(i) {
			t.Errorf("%s e2e matrix shard %q is not the contiguous range 0..%d", nightlyWorkflow, shards, len(shards)-1)

			break
		}
	}

	env := job.get("env")
	if got, want := env.get("E2E_SHARD_TOTAL").scalar, strconv.Itoa(len(shards)); got != want {
		t.Errorf("%s e2e E2E_SHARD_TOTAL = %q, want %q (the shard count)", nightlyWorkflow, got, want)
	}

	if got := env.get("E2E_SHARD_INDEX").scalar; got != "${{ matrix.shard }}" {
		t.Errorf("%s e2e E2E_SHARD_INDEX = %q, want the matrix shard", nightlyWorkflow, got)
	}
}

// The `if:` of the nightly suite jobs: each runs in one tier, and a scheduled
// run skips the suites once their commit, Nextflow release and tier passed.
const (
	nightlyDailyGate  = "${{ needs.gate.outputs.run == 'true' && needs.gate.outputs.tier == 'daily' }}"
	nightlyWeeklyGate = "${{ needs.gate.outputs.run == 'true' && needs.gate.outputs.tier == 'weekly' }}"
)

// nightlyHeavyJobs are the suite jobs of the weekly tier; every other suite
// job is daily.
var nightlyHeavyJobs = []string{"scale", "gendiff"}

// nightlyDiscoveryJobs report what they find to their own issue, so neither
// the pass record nor the failure report needs them: a finding must neither
// block a pass nor open the "Nightly CI is failing" issue.
var nightlyDiscoveryJobs = []string{"gendiff"}

// nightlyMarkGate lets the pass record run beside the other tier's skipped
// jobs, which the implicit success() would treat as a reason to skip it.
const nightlyMarkGate = "${{ !failure() && !cancelled() && needs.gate.outputs.run == 'true' }}"

// TestNightlySuitesWaitOnChangeGate requires every suite job to need the
// change gate and run only on its verdict, in its tier, and the pass record
// to need every suite job and run only when none failed, so a pass is
// recorded only when all of its tier's suites ran green.
func TestNightlySuitesWaitOnChangeGate(t *testing.T) {
	var suites []string

	var mark workflowJob

	for _, j := range workflowJobs(t, nightlyWorkflow) {
		switch j.id {
		case "gate", "report":
		case "mark":
			mark = j
		default:
			suites = append(suites, j.id)

			want := nightlyDailyGate
			if slices.Contains(nightlyHeavyJobs, j.id) {
				want = nightlyWeeklyGate
			}

			if !slices.Contains(j.needs, "gate") || j.cond != want {
				t.Errorf("job %q: needs %q, if %q; want needs gate, if %q", j.id, j.needs, j.cond, want)
			}
		}
	}

	if mark.id == "" {
		t.Fatalf("%s has no mark job", nightlyWorkflow)
	}

	for _, s := range suites {
		discovery := slices.Contains(nightlyDiscoveryJobs, s)

		switch needed := slices.Contains(mark.needs, s); {
		case !needed && !discovery:
			t.Errorf("mark job does not need suite job %q, so a pass could be recorded without it", s)
		case needed && discovery:
			t.Errorf("mark job needs discovery job %q, so a finding would block the pass record", s)
		}
	}

	if mark.cond != nightlyMarkGate {
		t.Errorf("mark job has if %q, want %q", mark.cond, nightlyMarkGate)
	}
}

// nightlyWeeklyCron is the schedule of the weekly tier.
const nightlyWeeklyCron = "17 6 * * 0"

// TestNightlyHeavyJobsRunWeekly runs the gate's tier step for every schedule
// and for a dispatch with and without `heavy`: only the weekly cron and the
// `heavy` input pick the weekly tier, which alone runs nightlyHeavyJobs, and
// the tier is part of the pass key, so one tier's pass never skips the other.
func TestNightlyHeavyJobsRunWeekly(t *testing.T) {
	requireTools(t, "bash")

	wf := readWorkflow(t, nightlyWorkflow)

	heavy := wf.get("on").get("workflow_dispatch").get("inputs").get("heavy")
	if scalarOf(heavy.get("type")) != "boolean" || scalarOf(heavy.get("default")) != "false" {
		t.Errorf("%s workflow_dispatch input heavy: want type boolean, default false", nightlyWorkflow)
	}

	type trigger struct{ event, schedule, heavy, want string }

	schedules := wf.get("on").get("schedule").items

	triggers := make([]trigger, 0, len(schedules)+2)
	triggers = append(triggers, trigger{"workflow_dispatch", "", "false", "daily"}, trigger{"workflow_dispatch", "", "true", "weekly"})

	crons := make([]string, 0, len(schedules))

	for _, s := range schedules {
		cron := scalarOf(s.get("cron"))
		crons = append(crons, cron)

		want := "daily"
		if cron == nightlyWeeklyCron {
			want = "weekly"
		}

		triggers = append(triggers, trigger{"schedule", cron, "", want})
	}

	if !slices.Contains(crons, nightlyWeeklyCron) || len(crons) < 2 {
		t.Errorf("%s schedules %q: want a daily cron and the weekly %q", nightlyWorkflow, crons, nightlyWeeklyCron)
	}

	steps := wf.get("jobs").get("gate").get("steps").items
	tierStep := steps[slices.IndexFunc(steps, func(s *yamlNode) bool { return scalarOf(s.get("id")) == "tier" })]

	for _, tr := range triggers {
		if got := runTierStep(t, scalarOf(tierStep.get("run")), tr.event, tr.schedule, tr.heavy); got != tr.want {
			t.Errorf("%s (schedule %q, heavy %q): tier %q, want %q", tr.event, tr.schedule, tr.heavy, got, tr.want)
		}
	}

	key := steps[slices.IndexFunc(steps, func(s *yamlNode) bool { return scalarOf(s.get("id")) == "key" })]
	if !strings.Contains(scalarOf(key.get("run")), "-$TIER\"") {
		t.Errorf("%s gate key does not end in the tier", nightlyWorkflow)
	}
}

// runTierStep runs the gate's tier script as Actions would for the trigger
// and returns the tier it outputs.
func runTierStep(t *testing.T, script, event, schedule, heavy string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "output")

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "EVENT="+event, "SCHEDULE="+schedule, "HEAVY="+heavy, "GITHUB_OUTPUT="+out)

	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tier step: %v\n%s", err, b)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimPrefix(strings.TrimSpace(string(data)), "tier=")
}

var scaleTestFunc = regexp.MustCompile(`(?m)^func (TestScale\w+)\(`)

// TestNightlyScaleMatrixRunsEveryShape requires the scale job's matrix to
// run every TestScale function in test/e2e/scale_test.go, one per job, so a
// new shape cannot go unrun.
func TestNightlyScaleMatrixRunsEveryShape(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(root, "test", "e2e", "scale_test.go"))
	if err != nil {
		t.Fatal(err)
	}

	funcs := scaleTestFunc.FindAllSubmatch(src, -1)

	want := make([]string, 0, len(funcs))
	for _, m := range funcs {
		want = append(want, string(m[1]))
	}

	job := readWorkflow(t, nightlyWorkflow).get("jobs").get("scale")
	shapes := job.get("strategy").get("matrix").get("shape").list()

	got := make([]string, 0, len(shapes))
	for _, s := range shapes {
		got = append(got, "TestScale"+s)
	}

	slices.Sort(want)
	slices.Sort(got)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("%s scale matrix shapes != TestScale functions (-want +got):\n%s", nightlyWorkflow, diff)
	}

	const run = "make test-scale SCALE_RUN='^TestScale${{ matrix.shape }}'"
	if !slices.ContainsFunc(job.get("steps").items, func(s *yamlNode) bool { return scalarOf(s.get("run")) == run }) {
		t.Errorf("%s scale job never runs %q", nightlyWorkflow, run)
	}
}

// nightlyArtifacts are the nightly jobs whose results are an artifact, with
// the dir each uploads.
var nightlyArtifacts = map[string]string{"scale": "artifacts/scale/", "gendiff": "artifacts/gen-diff/"}

// TestNightlyUploadsArtifacts requires each artifact job to upload its dir
// in a step that runs whatever the suite's outcome, so a failed night keeps
// the results that explain it.
func TestNightlyUploadsArtifacts(t *testing.T) {
	jobs := readWorkflow(t, nightlyWorkflow).get("jobs")

	for id, dir := range nightlyArtifacts {
		uploads := slices.ContainsFunc(jobs.get(id).get("steps").items, func(s *yamlNode) bool {
			return strings.HasPrefix(scalarOf(s.get("uses")), "actions/upload-artifact@") &&
				scalarOf(s.get("with").get("path")) == dir && scalarOf(s.get("if")) == "${{ always() }}"
		})
		if !uploads {
			t.Errorf("%s job %q has no `if: ${{ always() }}` upload-artifact step for %s", nightlyWorkflow, id, dir)
		}
	}
}

// TestNightlyGenDiffRunsRandomSeeds requires the gen-diff job to run make
// gen-diff on its default random seeds, under a time budget, with the mrp
// the differential needs.
func TestNightlyGenDiffRunsRandomSeeds(t *testing.T) {
	job := readWorkflow(t, nightlyWorkflow).get("jobs").get("gendiff")

	if scalarOf(job.get("env").get("GEN_BUDGET")) == "" {
		t.Errorf("%s gendiff job sets no GEN_BUDGET", nightlyWorkflow)
	}

	if job.get("env").get("GEN_SEEDS") != nil {
		t.Errorf("%s gendiff job sets GEN_SEEDS; the nightly runs the random default", nightlyWorkflow)
	}

	var runs, martian bool

	for _, s := range job.get("steps").items {
		run := scalarOf(s.get("run"))
		runs = runs || strings.HasPrefix(run, "make gen-diff ") && !strings.Contains(run, "GEN_SEEDS")
		martian = martian || scalarOf(s.get("uses")) == "./.github/actions/setup-martian"
	}

	if !runs || !martian {
		t.Errorf("%s gendiff job: runs make gen-diff without GEN_SEEDS %v, sets up mrp %v; want both", nightlyWorkflow, runs, martian)
	}
}

// nightlyDivergenceTitle is the exact title, and so the dedupe key, of the
// issue the gen-diff job files its divergences on.
const nightlyDivergenceTitle = "Generative differential: new divergences"

// fakeGh stands in for gh: `issue list` prints $FAKE_ISSUE, and every other
// call appends its arguments, one per line, to $FAKE_GH_LOG.
const fakeGh = `#!/bin/bash
if [ "$1 $2" = "issue list" ]; then echo "$FAKE_ISSUE"; exit 0; fi
printf '%s\n' "$@" >> "$FAKE_GH_LOG"
`

// TestNightlyGenDiffFilesDivergences runs the gen-diff job's issue step
// against a fake gh: with no open tracking issue it opens one, with one it
// comments, and the body names the run, each seed with its finding keys and
// the artifact. Only that job and the report job may write issues.
func TestNightlyGenDiffFilesDivergences(t *testing.T) {
	requireTools(t, "bash", "jq")

	wf := readWorkflow(t, nightlyWorkflow)
	job := wf.get("jobs").get("gendiff")

	if wf.get("permissions").get("issues") != nil {
		t.Errorf("%s grants issues at the workflow level", nightlyWorkflow)
	}

	for _, id := range wf.get("jobs").keys {
		writes := scalarOf(wf.get("jobs").get(id).get("permissions").get("issues")) == "write"
		if writes != (id == "gendiff" || id == "report") {
			t.Errorf("%s job %q issues: write = %v, want it only on gendiff and report", nightlyWorkflow, id, writes)
		}
	}

	steps := job.get("steps").items

	i := slices.IndexFunc(steps, func(s *yamlNode) bool { return strings.Contains(scalarOf(s.get("run")), "gh issue") })
	if i < 0 {
		t.Fatalf("%s gendiff job has no issue step", nightlyWorkflow)
	}

	if cond := scalarOf(steps[i].get("if")); !strings.Contains(cond, "!cancelled()") || !strings.Contains(cond, "refs/heads/main") {
		t.Errorf("gendiff issue step if %q: want it after a failed run too, and only on main", cond)
	}

	findings := filepath.Join(t.TempDir(), "divergences.jsonl")
	lines := `{"seed":42,"findings":["default outs:r0","native abi:ST0 main"],"fixture":"gen_0123456789ab"}` + "\n"

	if err := os.WriteFile(findings, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, issue := range []string{"", "17"} {
		log := runIssueStep(t, scalarOf(steps[i].get("run")), findings, issue)

		want := []string{"issue", "create", "--title", nightlyDivergenceTitle, "--body"}
		if issue != "" {
			want = []string{"issue", "comment", issue, "--body"}
		}

		if !strings.HasPrefix(log, strings.Join(want, "\n")+"\n") {
			t.Errorf("open issue %q: gh got\n%s\nwant it to start %q", issue, log, want)
		}

		for _, part := range []string{"https://run/1", "https://artifact/2", "seed 42", "`gen_0123456789ab`", "`default outs:r0`, `native abi:ST0 main`"} {
			if !strings.Contains(log, part) {
				t.Errorf("open issue %q: body lacks %q:\n%s", issue, part, log)
			}
		}
	}
}

// runIssueStep runs the gen-diff issue step with a fake gh on PATH, the
// findings file and the open issue number (or "") gh reports, and returns
// the arguments of gh's create or comment call.
func runIssueStep(t *testing.T, script, findings, issue string) string {
	t.Helper()

	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "gh.log")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGh), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_GH_LOG="+log, "FAKE_ISSUE="+issue,
		"GEN_FINDINGS="+findings, "RUN_URL=https://run/1", "ARTIFACT_URL=https://artifact/2", "GITHUB_SHA=abc123")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("issue step: %v\n%s", err, out)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// scalarOf is n's scalar, or "" for an absent node.
func scalarOf(n *yamlNode) string {
	if n == nil {
		return ""
	}

	return n.scalar
}
