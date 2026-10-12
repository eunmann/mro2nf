package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// requiredContexts is the branch-protection required status-check set. GitHub
// matches a required context by check name, from ANY workflow, and accepts the
// first success it sees: a second workflow posting one of these names bypasses
// the real job (#280). So pr-validation.yml must be their only source, it must
// run on every PR so they always report (#182), and a docs-only PR may skip
// them only through a skipped-by-`if:` job, which GitHub counts as passing.
var requiredContexts = []string{
	"Lint",
	"Build",
	"Unit tests",
	"E2E (Nextflow vs mrp goldens)",
	"Lint generated Nextflow",
	"Bench (data movement gate)",
}

// internalJobs are the pr-validation.yml jobs that are deliberately not
// required contexts.
var internalJobs = []string{
	"Detect code changes",
	"E2E shard config",
	"E2E shard ${{ matrix.shard }}",
}

const gateWorkflow = "pr-validation.yml"

// workflowsDir is the repo's workflow dir, relative to this package's dir,
// where go test runs it: these tests read files only, so they need no
// harness and run under make test.
var workflowsDir = filepath.Join("..", "..", ".github", "workflows")

type workflowJob struct {
	id    string
	name  string
	needs []string
	cond  string
}

func readWorkflow(t *testing.T, file string) *yamlNode {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(workflowsDir, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	doc, err := parseYAML(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	return doc
}

// workflowJobs lists a workflow's jobs; a job without `name:` reports its
// check under its job id.
func workflowJobs(t *testing.T, file string) []workflowJob {
	t.Helper()

	jobs := readWorkflow(t, file).get("jobs")
	if jobs == nil || len(jobs.keys) == 0 {
		t.Fatalf("%s: no jobs", file)
	}

	out := make([]workflowJob, 0, len(jobs.keys))
	for _, id := range jobs.keys {
		j := jobs.get(id)
		name := id
		if n := j.get("name"); n != nil {
			name = n.scalar
		}

		var cond string
		if c := j.get("if"); c != nil {
			cond = c.scalar
		}

		out = append(out, workflowJob{id: id, name: name, needs: j.get("needs").list(), cond: cond})
	}

	return out
}

// TestRequiredContextsOnlyFromPRValidation fails if any other workflow emits a
// required context name: its check would satisfy branch protection before (or
// instead of) the real job, as pr-validation-skip.yml's did (#280).
func TestRequiredContextsOnlyFromPRValidation(t *testing.T) {
	var files []string

	for _, pattern := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(workflowsDir, pattern))
		if err != nil {
			t.Fatalf("glob workflows: %v", err)
		}

		files = append(files, m...)
	}

	if len(files) < 2 {
		t.Fatalf("found %d workflow files; the glob is wrong", len(files))
	}

	for _, path := range files {
		file := filepath.Base(path)
		if file == gateWorkflow {
			continue
		}

		for _, j := range workflowJobs(t, file) {
			if slices.Contains(requiredContexts, j.name) {
				t.Errorf("%s job %q emits required context %q; only %s may", file, j.id, j.name, gateWorkflow)
			}
		}
	}
}

// TestRequiredContextsExact pins pr-validation.yml's job names to the required
// set plus the internal jobs, each name on exactly one job, so a rename or an
// added job forces a decision about branch protection.
func TestRequiredContextsExact(t *testing.T) {
	jobs := workflowJobs(t, gateWorkflow)

	got := make([]string, 0, len(jobs))
	for _, j := range jobs {
		got = append(got, j.name)
	}

	want := slices.Concat(requiredContexts, internalJobs)
	slices.Sort(got)
	slices.Sort(want)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("%s job names (-want +got):\n%s", gateWorkflow, diff)
	}
}

// TestRequiredContextsGatedOnChanges checks every required job skips only on
// the `changes` job's successful docs-only verdict, under an explicit status
// function: with the implicit success(), a failed `changes` would skip every
// required job, and GitHub counts those skips as passing.
func TestRequiredContextsGatedOnChanges(t *testing.T) {
	jobs := workflowJobs(t, gateWorkflow)
	if !slices.ContainsFunc(jobs, func(j workflowJob) bool { return j.id == "changes" }) {
		t.Fatalf("%s has no `changes` job", gateWorkflow)
	}

	for _, j := range jobs {
		if !slices.Contains(requiredContexts, j.name) {
			continue
		}

		if !slices.Contains(j.needs, "changes") {
			t.Errorf("job %q needs %v, want it to include changes", j.id, j.needs)
		}

		if !slices.Contains(docsOnlyGates, j.cond) {
			t.Errorf("job %q if %q, want one of %q", j.id, j.cond, docsOnlyGates)
		}
	}
}

// docsOnlyGates are the only accepted `if:` of a required job, pinned whole:
// a dropped negation or a `||` for the inner `&&` still mentions every
// operand, yet skips the job on every code PR.
var docsOnlyGates = []string{
	"${{ !cancelled() && !(needs.changes.result == 'success' && needs.changes.outputs.code == 'false') }}",
	"${{ always() && !(needs.changes.result == 'success' && needs.changes.outputs.code == 'false') }}",
}

// TestRequiredContextsReportOnEveryPR fails if pr-validation.yml's
// pull_request trigger regains a path or branch filter: a PR it filters out
// gets no required contexts and deadlocks (#182).
func TestRequiredContextsReportOnEveryPR(t *testing.T) {
	on := readWorkflow(t, gateWorkflow).get("on")

	pr := on.get("pull_request")
	if pr == nil {
		t.Fatalf("%s does not trigger on pull_request", gateWorkflow)
	}

	for _, filter := range []string{"paths", "paths-ignore", "branches", "branches-ignore"} {
		if pr.get(filter) != nil {
			t.Errorf("%s pull_request has a %s filter", gateWorkflow, filter)
		}
	}
}
