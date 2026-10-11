//go:build e2e

package e2e

import (
	"os"
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
		if j.cond == nightlyFailureGate {
			reporters = append(reporters, j)
		} else {
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
		t.Errorf("report job %q must need every other job (-want +got):\n%s", r.id, diff)
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

// nightlyChangeGate is the `if:` of every nightly suite job: a scheduled run
// skips the suites once their commit and Nextflow release have passed.
const nightlyChangeGate = "${{ needs.gate.outputs.run == 'true' }}"

// TestNightlySuitesWaitOnChangeGate requires every suite job to need the
// change gate and run only on its verdict, and the pass record to need every
// suite job, so a pass is recorded only when all of them ran green.
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

			if !slices.Contains(j.needs, "gate") || j.cond != nightlyChangeGate {
				t.Errorf("job %q: needs %q, if %q; want needs gate, if %q", j.id, j.needs, j.cond, nightlyChangeGate)
			}
		}
	}

	if mark.id == "" {
		t.Fatalf("%s has no mark job", nightlyWorkflow)
	}

	for _, s := range suites {
		if !slices.Contains(mark.needs, s) {
			t.Errorf("mark job does not need suite job %q, so a pass could be recorded without it", s)
		}
	}

	if mark.cond != "" {
		t.Errorf("mark job has if %q; it must keep the implicit success()", mark.cond)
	}
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
var nightlyArtifacts = map[string]string{"scale": "artifacts/scale/"}

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

// scalarOf is n's scalar, or "" for an absent node.
func scalarOf(n *yamlNode) string {
	if n == nil {
		return ""
	}

	return n.scalar
}
