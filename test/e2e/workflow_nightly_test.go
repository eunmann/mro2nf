//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	nightlyWorkflow = "nightly.yml"

	// nightlyJobPrefix starts every nightly job name. No required context
	// starts with it, so no matrix expansion of a nightly name can post one.
	nightlyJobPrefix = "Nightly"

	nightlyFailureGate = "${{ failure() }}"
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
// failure issue: it runs only on failure, after every other job, and holds
// the issues permission its gh calls need.
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

// TestNightlyNextflowFloor requires the nightly e2e matrix to run the
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

	matrix := readWorkflow(t, nightlyWorkflow).get("jobs").get("e2e").get("strategy").get("matrix")
	got := matrix.get("nextflow").list()

	for _, want := range []string{string(m[1]), "latest-stable"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s e2e matrix nextflow %q lacks %q", nightlyWorkflow, got, want)
		}
	}
}
