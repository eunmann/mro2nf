//go:build e2e

package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/job"
)

// failfileFixture is Martian's fork_test, whose ADD_KEY1 reads the file
// fail1 in $FAILFILE_DIR when there is one, deletes it, and sends itself the
// signal the file names, or raises when it names none.
const failfileFixture = "fork_test"

// failfileVariant is a run of fork_test as a script in Martian's
// test/fork_test runs it: with fail1 holding fail1, and under mrp
// --autoretry and Nextflow's process.maxRetries set to autoretry when it is
// not 0, the mapping docs/usage.md gives. first is
// the outcome both runners' first run must give. rerun reruns a failed run,
// mrp on its pipestance and Nextflow with -resume. The run that succeeds,
// the first or the rerun, is diffed as TestMrpDiff diffs fork_test, and
// retried is how many attempts of the Nextflow one fail, each ADD_KEY1's
// with mre's transient exit.
type failfileVariant struct {
	name      string
	fail1     string
	autoretry int
	first     runOutcome
	rerun     bool
	retried   int
}

// failfileVariants are the scripts of Martian's test/fork_test other than
// run_test.sh, which TestMrpDiff/fork_test runs, and autoretry_remote_pass.sh,
// whose fake_remote job manager no profile stands for and which Martian's CI
// does not run.
var failfileVariants = []failfileVariant{
	// run_fail1_test.sh: ADD_KEY1 raises, which fails the run.
	{name: "fail1", first: failedIn(failError, "ADD_KEY1")},
	// autoretry_fail_test.sh: an exception is no transient failure, so
	// --autoretry does not retry it.
	{name: "autoretry_fail", autoretry: 1, first: failedIn(failError, "ADD_KEY1")},
	// autoretry_pass_test.sh: ADD_KEY1 hangs up on itself, and its retry
	// succeeds.
	{name: "autoretry_pass", fail1: "1", autoretry: 1, retried: 1},
	// retry_test.sh: ADD_KEY1 raises, and a rerun of the failed pipestance
	// succeeds.
	{name: "retry", first: failedIn(failError, "ADD_KEY1"), rerun: true},
}

// TestMrpDiffFailfile runs each failfileVariants entry under real mrp and
// under transpiled Nextflow, each run with its own FAILFILE_DIR, which the
// Nextflow tasks get from a -c config's env scope. Both first runs must give
// the variant's outcome and delete fail1, so it reached ADD_KEY1 under both.
// A failed run is compared as TestMrpDiffFailureParity compares one, and the
// run that succeeds must publish what mrp's does, with the same stage ABI.
func TestMrpDiffFailfile(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java", "python3")

	bin := martianBin(t)

	for _, v := range failfileVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			tmp, proj := t.TempDir(), transpile(t, failfileFixture)
			v.runMrp(t, bin, tmp)
			v.runNextflow(t, proj)

			if !v.first.Failed || v.rerun {
				compareMrpToNextflow(t, failfileFixture, tmp, proj)
				compareStageABI(t, failfileFixture, tmp, proj)
			}
		})
	}
}

// runMrp runs the variant under mrp in tmp, and reruns a failed run on its
// pipestance when the variant does.
func (v failfileVariant) runMrp(t *testing.T, bin, tmp string) {
	t.Helper()

	dir := failfileDir(t, v.fail1)

	var flags []string
	if v.autoretry > 0 {
		flags = append(flags, fmt.Sprintf("--autoretry=%d", v.autoretry))
	}

	run := func() runOutcome {
		_, err := execMrpVDR(t.Context(), bin, filepath.Join(root, "testdata", failfileFixture), "pipeline.mro", tmp,
			"disable", []string{"FAILFILE_DIR=" + dir}, flags...)

		o, err := mrpRunEnded(err, tmp)
		if err != nil {
			t.Fatal(err)
		}

		return o
	}

	if o := run(); !o.equal(v.first) {
		t.Fatalf("mrp's run: %s, want %s\n%s", o, v.first, tailFile(filepath.Join(tmp, "mrp.log")))
	}

	assertFailfileUsed(t, "mrp", dir)

	if v.rerun {
		if o := run(); o.Failed {
			t.Fatalf("mrp's rerun: %s\n%s", o, tailFile(filepath.Join(tmp, "mrp.log")))
		}
	}
}

// runNextflow runs the variant's Nextflow project proj, and reruns a failed
// run with -resume when the variant does.
func (v failfileVariant) runNextflow(t *testing.T, proj string) {
	t.Helper()

	dir := failfileDir(t, v.fail1)

	config := fmt.Sprintf("env.FAILFILE_DIR = '%s'\n", dir)
	if v.autoretry > 0 {
		config += fmt.Sprintf("process.maxRetries = %d\n", v.autoretry)
	}

	args := append(mrpPoolArgs(t, proj), "-c", writeTestFile(t, filepath.Join(proj, "failfile.config"), config))

	o, attempts := nextflowRunOutcome(t, proj, args...)
	if !o.equal(v.first) {
		t.Fatalf("mro2nf's run: %s, want %s\n%s", o, v.first, tailFile(filepath.Join(proj, "run.log")))
	}

	assertFailfileUsed(t, "mro2nf", dir)

	if v.first.Failed {
		for _, r := range outcomeRuleReports(mustAllowlist(t), failfileFixture, attempts, 0) {
			t.Error(r)
		}
	}

	switch {
	case v.rerun:
		if err := runNextflow(t, proj, append(args, "-resume")...); err != nil {
			t.Fatalf("mro2nf's rerun: %v", err)
		}
	case !v.first.Failed:
		assertRetried(t, readTrace(t, filepath.Join(proj, "trace.txt")), v.retried)
	}
}

// failfileDir is a new FAILFILE_DIR holding fail1 with content.
func failfileDir(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "fail1"), content)

	return dir
}

// assertFailfileUsed fails unless the runner's run deleted fail1 from dir,
// as ADD_KEY1 does once it reads it.
func assertFailfileUsed(t *testing.T, runner, dir string) {
	t.Helper()

	if fileExists(filepath.Join(dir, "fail1")) {
		t.Errorf("%s's run left %s, so ADD_KEY1 never read it", runner, filepath.Join(dir, "fail1"))
	}
}

// assertRetried fails unless rows, a successful run's trace, hold want
// attempts that did not complete, each ADD_KEY1's with mre's transient exit,
// the failure mrp retries.
func assertRetried(t *testing.T, rows []traceRow, want int) {
	t.Helper()

	failed := 0

	for _, r := range rows {
		if r.Status == "COMPLETED" {
			continue
		}

		failed++

		if !strings.Contains(r.Name, "ADD_KEY1") || r.Exit != job.TransientExitCode {
			t.Errorf("attempt %s %s (exit %d); want ADD_KEY1's transient failure (exit %d)", r.Name, r.Status, r.Exit, job.TransientExitCode)
		}
	}

	if failed != want {
		t.Errorf("%d attempts failed, want %d", failed, want)
	}
}
