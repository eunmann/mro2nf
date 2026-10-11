package e2e

// Failure parity against real mrp (#349): for a fixture mrp fails on, whether
// the run fails is part of the observable contract, so mrp fails exactly when
// mro2nf fails, and both blame the same stages the same way.
//
// An outcome is whether the run failed plus, per failed stage, its name and
// whether it was an ASSERT or an error. mrp marks an ASSERT with _assert and
// any other failure with _errors, in the phase dir when stage code failed and in
// the fork dir when mrp's own checks did. mro2nf marks an ASSERT with exit 42
// (shim.AssertExitCode) and any other failure with another exit, and mre names
// the stage in the task's _mre_stage. Leaving the rest out excuses real
// differences, so each is an allowlist rule the comparison applies only while
// its entry exists (divergences.json). Message text differs by design: mre
// words its errors itself and Nextflow adds context. The phase is not reliable
// either, because mrp records an output-validation failure at the fork while
// mre records it in the task that ran the phase, and neither is the chunk
// index (RuleFailureStageAndKind). mro2nf retries an error and mrp does not,
// so a retried stage task's failures count once (RuleRetriedAttempts).
//
// This file carries no build tag so its unit tests run under `make test`;
// mrpdiff_test.go drives it against live runs.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/test/divergence"
)

const (
	failAssert = divergence.FailAssert
	failError  = divergence.FailError
)

// mrpFailureFiles maps the metadata files mrp writes for a failed stage to the
// failure kind each records.
var mrpFailureFiles = map[string]string{"_assert": failAssert, "_errors": failError}

// stageFailure is one stage a run blamed. An empty Stage is a failure no stage
// task owns, such as an error in the Nextflow driver or in a plumbing task.
type stageFailure struct {
	Kind  string
	Stage string
}

func (f stageFailure) String() string {
	if f.Stage == "" {
		return f.Kind + " outside any stage"
	}

	return f.Kind + " in " + f.Stage
}

// runOutcome is how a run ended: success, or failure with the distinct stage
// failures it reported, sorted. The zero value is a successful run.
type runOutcome struct {
	Failed   bool
	Failures []stageFailure
}

// failedIn is the outcome of a run that failed in exactly one stage.
func failedIn(kind, stage string) runOutcome {
	return runOutcome{Failed: true, Failures: []stageFailure{{Kind: kind, Stage: stage}}}
}

// failedWith is the outcome of a failed run that reported failures, each
// distinct failure once.
func failedWith(failures []stageFailure) runOutcome {
	if len(failures) == 0 {
		return runOutcome{Failed: true}
	}

	failures = slices.Clone(failures)
	slices.SortFunc(failures, func(a, b stageFailure) int {
		return strings.Compare(a.Stage+"\x00"+a.Kind, b.Stage+"\x00"+b.Kind)
	})

	return runOutcome{Failed: true, Failures: slices.Compact(failures)}
}

func (o runOutcome) String() string {
	if !o.Failed {
		return "success"
	}

	if len(o.Failures) == 0 {
		return "failure blaming no stage"
	}

	parts := make([]string, len(o.Failures))
	for i, f := range o.Failures {
		parts[i] = f.String()
	}

	return "failure: " + strings.Join(parts, "; ")
}

func (o runOutcome) equal(p runOutcome) bool {
	return o.Failed == p.Failed && slices.Equal(o.Failures, p.Failures)
}

// mrpFailedOutcome classifies a pipestance mrp exited non-zero on, from the
// _assert and _errors files it left. A missing pipestance (a compile error) is
// a failure blaming no stage.
func mrpFailedOutcome(ps string) (runOutcome, error) {
	if _, err := os.Stat(ps); errors.Is(err, fs.ErrNotExist) {
		return failedWith(nil), nil
	}

	var failures []stageFailure

	err := filepath.WalkDir(ps, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		kind, ok := mrpFailureFiles[d.Name()]
		if !ok || !d.Type().IsRegular() {
			return nil
		}

		fork := filepath.Dir(p)
		if mrpPhaseDir.MatchString(filepath.Base(fork)) {
			fork = filepath.Dir(fork)
		}

		stage, err := mrpStageName(fork)
		if err != nil {
			return err
		}

		failures = append(failures, stageFailure{Kind: kind, Stage: stage})

		return nil
	})
	if err != nil {
		return runOutcome{}, fmt.Errorf("classify mrp failure: %w", err)
	}

	return failedWith(failures), nil
}

// failedAttempt is a FAILED row of a Nextflow trace: the task's work dir hash
// prefix (`ab/cdef12`) and its exit status.
type failedAttempt struct {
	Hash string
	Exit int
}

// nextflowFailedOutcome classifies a Nextflow run that exited non-zero from
// its failed task attempts. A failed task without an _mre_stage stamp ran no
// stage, so it blames none.
func nextflowFailedOutcome(work string, failed []failedAttempt) (runOutcome, error) {
	failures := make([]stageFailure, 0, len(failed))

	for _, a := range failed {
		stage, err := taskStage(work, a.Hash)
		if err != nil {
			return runOutcome{}, err
		}

		kind := failError
		if a.Exit == shim.AssertExitCode {
			kind = failAssert
		}

		failures = append(failures, stageFailure{Kind: kind, Stage: stage})
	}

	return failedWith(failures), nil
}

// taskStage is the stage the task at work/<hash>* ran, or "" for a task that
// ran no stage phase.
func taskStage(work, hash string) (string, error) {
	dirs, err := filepath.Glob(filepath.Join(work, filepath.FromSlash(hash)+"*"))
	if err != nil {
		return "", fmt.Errorf("glob task %s: %w", hash, err)
	}

	if len(dirs) != 1 {
		return "", fmt.Errorf("%w: %d work dirs for task %s", errABIMeta, len(dirs), hash)
	}

	stamps, err := filepath.Glob(filepath.Join(dirs[0], "*", mreStageFile))
	if err != nil {
		return "", fmt.Errorf("glob stage stamps: %w", err)
	}

	switch len(stamps) {
	case 0:
		return "", nil
	case 1:
		stage, err := os.ReadFile(stamps[0])
		if err != nil {
			return "", fmt.Errorf("read stage name: %w", err)
		}

		return string(stage), nil
	default:
		return "", fmt.Errorf("%w: task %s ran %d stage phases", errABIMeta, hash, len(stamps))
	}
}

// outcomeDivergence is the allowlist entry a case names: the outcome mro2nf
// gives instead of mrp's on the case's fixture.
type outcomeDivergence struct {
	id string
	nf runOutcome
}

// outcomeOf is the run outcome an allowlist outcome matcher records.
func outcomeOf(m divergence.OutcomeMatcher) runOutcome {
	if !m.Failed {
		return runOutcome{}
	}

	failures := make([]stageFailure, len(m.Failures))
	for i, f := range m.Failures {
		failures[i] = stageFailure{Kind: f.Kind, Stage: f.Stage}
	}

	return failedWith(failures)
}

// mrpOutcomeCase is a fixture whose run outcome, not its outputs, is compared
// with mrp's: mrp fails it by design, or mro2nf's outcome is a known
// divergence from mrp's, recorded by the allowlist entry divergence names.
type mrpOutcomeCase struct {
	name       string
	mrp        runOutcome
	divergence string
}

// errOutcomeCase reports a malformed mrpOutcomeCases entry.
var errOutcomeCase = errors.New("outcome case")

// validate resolves the case's divergence against allow. It rejects a case
// that belongs in mrpDiffCases, because both sides succeed on it, and a
// divergence that is no outcome entry or records mrp's own outcome.
func (c mrpOutcomeCase) validate(allow divergence.Allowlist) (*outcomeDivergence, error) {
	if c.divergence == "" {
		if !c.mrp.Failed {
			return nil, fmt.Errorf("%w %q: mrp succeeds and no divergence is recorded; diff it in mrpDiffCases", errOutcomeCase, c.name)
		}

		return nil, nil
	}

	e, err := allow.Outcome(c.divergence)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", errOutcomeCase, c.name, err)
	}

	d := &outcomeDivergence{id: e.ID, nf: outcomeOf(*e.Outcome)}
	if d.nf.equal(c.mrp) {
		return nil, fmt.Errorf("%w %q: divergence %s records mrp's own outcome", errOutcomeCase, c.name, d.id)
	}

	return d, nil
}

// check compares the live outcomes with the case: mrp must give the recorded
// outcome and mro2nf the same one. With a divergence d, mro2nf must give the
// divergent outcome instead, so a fix fails the check until the entry is gone.
func (c mrpOutcomeCase) check(d *outcomeDivergence, mrp, nf runOutcome) []string {
	var bad []string

	if !mrp.equal(c.mrp) {
		bad = append(bad, fmt.Sprintf("mrp outcome is %s; the case records %s", mrp, c.mrp))
	}

	if d == nil {
		if !nf.equal(c.mrp) {
			bad = append(bad, fmt.Sprintf("mro2nf outcome is %s; mrp's is %s", nf, c.mrp))
		}

		return bad
	}

	switch {
	case nf.equal(c.mrp):
		bad = append(bad, fmt.Sprintf("divergence %s is gone: mro2nf now matches mrp (%s); delete it, "+
			"and move a fixture both sides succeed on to mrpDiffCases", d.id, nf))
	case !nf.equal(d.nf):
		bad = append(bad, fmt.Sprintf("mro2nf outcome is %s; mrp's is %s and divergence %s records %s",
			nf, c.mrp, d.id, d.nf))
	}

	return bad
}

// outcomeRuleReports reports each outcome comparator rule fixture's run needs
// that allow has no entry for, and each issue-tracked rule entry scoped to
// fixture that the run does not need. attempts names the task of every attempt
// in the Nextflow run's trace; a name seen twice is a retried task.
func outcomeRuleReports(allow divergence.Allowlist, fixture string, attempts []string) []string {
	bad := outcomeRuleReportsUnder(allow, fixture, attempts)

	for _, e := range allow.ScopedRules(divergence.KindOutcome, fixture) {
		if slices.Equal(outcomeRuleReportsUnder(allow.Without(e.ID), fixture, attempts), bad) {
			bad = append(bad, fmt.Sprintf("allowlist entry %s excuses nothing in %s; delete it or narrow its fixtures", e.ID, fixture))
		}
	}

	return bad
}

func outcomeRuleReportsUnder(allow divergence.Allowlist, fixture string, attempts []string) []string {
	var bad []string

	if !allow.Excuses(divergence.RuleFailureStageAndKind, fixture) {
		bad = append(bad, "the outcome comparison reads only each failure's stage and kind, "+
			"and no allowlist entry excuses leaving out its message, phase and chunk")
	}

	if retried := retriedTasks(attempts); len(retried) > 0 && !allow.Excuses(divergence.RuleRetriedAttempts, fixture) {
		bad = append(bad, fmt.Sprintf("mro2nf attempted %s more than once, where mrp does not retry a stage error, "+
			"and no allowlist entry excuses it", strings.Join(retried, ", ")))
	}

	return bad
}

// retriedTasks returns, sorted, the task names attempts lists more than once.
func retriedTasks(attempts []string) []string {
	count := map[string]int{}
	for _, name := range attempts {
		count[name]++
	}

	var retried []string

	for name, n := range count {
		if n > 1 {
			retried = append(retried, name)
		}
	}

	slices.Sort(retried)

	return retried
}
