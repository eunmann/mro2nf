package e2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/test/divergence"
	"github.com/google/go-cmp/cmp"
)

// TestMrpFailedOutcome checks a failure is attributed to the stage of the fork
// it was recorded in, whether mrp wrote it in a phase dir (stage code) or in
// the fork dir (mrp's own checks), and that _assert and _errors keep apart.
func TestMrpFailedOutcome(t *testing.T) {
	ps := t.TempDir()
	writeMrpSource(t, ps, "BOOM", "UP")
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "_invocation"), "call BOOM(\n    x = 1,\n)\n")
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "chnk0-u1c28cb18e6", "_assert"), "boom")
	writeABITestFile(t, filepath.Join(ps, "P", "UP", "fork0", "_invocation"), "call UP as UPPER()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "UP", "fork0", "_errors"), "missing key")
	writeABITestFile(t, filepath.Join(ps, "P", "UP", "fork1", "_invocation"), "call UP as UPPER()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "UP", "fork1", "split-u1ce7cb18e6", "_errors"), "bad split")

	if err := os.Symlink("chnk0-u1c28cb18e6", filepath.Join(ps, "P", "BOOM", "fork0", "chnk0")); err != nil {
		t.Fatal(err)
	}

	got, err := mrpFailedOutcome(ps)
	if err != nil {
		t.Fatal(err)
	}

	want := runOutcome{Failed: true, Failures: []stageFailure{{Kind: failAssert, Stage: "BOOM"}, {Kind: failError, Stage: "UP"}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("outcome (-want +got):\n%s", diff)
	}
}

// TestMrpFailedOutcomeKilledJob checks a job mrp killed while stopping on
// another stage's failure is not blamed: only the stage that failed is.
func TestMrpFailedOutcomeKilledJob(t *testing.T) {
	ps := t.TempDir()
	writeMrpSource(t, ps, "BOOM", "SLOW", "EARLY", "STAMPED")
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "_invocation"), "call BOOM()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "chnk0-u1", "_errors"), "Traceback ...\nRuntimeError: boom")
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "chnk1-u1", "_errors"), "Caught signal terminated")
	writeABITestFile(t, filepath.Join(ps, "P", "SLOW", "fork0", "_invocation"), "call SLOW()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "SLOW", "fork0", "chnk0-u2", "_errors"), "Caught signal terminated")
	writeABITestFile(t, filepath.Join(ps, "P", "EARLY", "fork0", "_invocation"), "call EARLY()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "EARLY", "fork0", "chnk0-u3", "_errors"), "signal: terminated")
	writeABITestFile(t, filepath.Join(ps, "P", "STAMPED", "fork0", "_invocation"), "call STAMPED()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "STAMPED", "fork0", "chnk0-u4", "_errors"), "2026-10-11 22:41:07 Caught signal interrupt")

	got, err := mrpFailedOutcome(ps)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(failedIn(failError, "BOOM"), got); diff != "" {
		t.Errorf("outcome (-want +got):\n%s", diff)
	}
}

// writeMrpSource writes the pipestance source mrp keeps at ps/_mrosource:
// pipeline P calling each of stages.
func writeMrpSource(t *testing.T, ps string, stages ...string) {
	t.Helper()

	var decls, calls strings.Builder

	for _, s := range stages {
		fmt.Fprintf(&decls, "stage %s(\n    src py \"stages/%s\",\n)\n\n", s, strings.ToLower(s))
		fmt.Fprintf(&calls, "    call %s()\n\n", s)
	}

	writeABITestFile(t, filepath.Join(ps, "_mrosource"), decls.String()+"pipeline P()\n{\n"+calls.String()+"    return ()\n}\n\ncall P()\n")
}

// TestMrpFailedOutcomePipelineFork checks a failure mrp records in a mapped
// pipeline's fork, resolving its bindings, blames no stage
// (map_pipe_nested: OUTER/INNER/fork0/_errors, `map call INNER(` in its
// _invocation).
func TestMrpFailedOutcomePipelineFork(t *testing.T) {
	ps := t.TempDir()
	writeABITestFile(t, filepath.Join(ps, "_mrosource"), "stage WORK(\n    in  int x,\n    src py \"stages/work\",\n)\n\n"+
		"pipeline INNER(\n    in  int x,\n)\n{\n    call WORK(\n        x = self.x,\n    )\n\n    return ()\n}\n\n"+
		"pipeline P()\n{\n    map call INNER(\n        x = split [1, 2],\n    )\n\n    return ()\n}\n\ncall P()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "INNER", "fork0", "_invocation"), "@include \"pipeline.mro\"\n\nmap call INNER(\n    x = split [1, 2],\n)\n")
	writeABITestFile(t, filepath.Join(ps, "P", "INNER", "fork0", "_errors"), "could not match reference to a specific fork")

	got, err := mrpFailedOutcome(ps)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(failedIn(failError, ""), got); diff != "" {
		t.Errorf("outcome (-want +got):\n%s", diff)
	}
}

// TestMrpFailedOutcomeNoPipestance checks a failed mrp that left no pipestance
// (a compile error) is a failure blaming no stage.
func TestMrpFailedOutcomeNoPipestance(t *testing.T) {
	got, err := mrpFailedOutcome(filepath.Join(t.TempDir(), "mrp"))
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(runOutcome{Failed: true}, got); diff != "" {
		t.Errorf("outcome (-want +got):\n%s", diff)
	}
}

// TestMrpFailedOutcomeUnattributable checks a failure file in a dir mrp left
// no _invocation in fails the classification instead of being dropped.
func TestMrpFailedOutcomeUnattributable(t *testing.T) {
	ps := t.TempDir()
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "_invocation"), "call BOOM()\n")
	writeABITestFile(t, filepath.Join(ps, "P", "BOOM", "fork0", "_assert"), "boom")
	writeABITestFile(t, filepath.Join(ps, "P", "fork0", "_errors"), "pipeline failed")

	got, err := mrpFailedOutcome(ps)
	if err == nil {
		t.Fatalf("outcome %s, want an error for the unattributable _errors", got)
	}

	if !mrpFailedOutsideStages(err) {
		t.Errorf("err = %v, want a failure outside any stage", err)
	}
}

// TestMrpFailedOutsideStagesIsNarrow checks a missing file other than a
// fork's _invocation, such as an mrp binary that is not there, is not taken
// for mrp failing outside any stage.
func TestMrpFailedOutsideStagesIsNarrow(t *testing.T) {
	err := fmt.Errorf("run mrp: %w", &fs.PathError{Op: "fork/exec", Path: "/no/mrp", Err: fs.ErrNotExist})
	if mrpFailedOutsideStages(err) {
		t.Errorf("mrpFailedOutsideStages(%v) = true, want false", err)
	}
}

// TestNextflowFailedOutcome checks each failed attempt is attributed to the
// stage its task ran, exit 42 as an ASSERT and any other exit as an error,
// with retried attempts of one stage counted once and a plumbing task blaming
// no stage.
func TestNextflowFailedOutcome(t *testing.T) {
	work := t.TempDir()
	writeABITestFile(t, filepath.Join(work, "4a", "5cab94aa", "join", mreStageFile), "JOIN_BOOM")
	writeABITestFile(t, filepath.Join(work, "1b", "0000aa11", "main", mreStageFile), "RAISE")
	writeABITestFile(t, filepath.Join(work, "1c", "0000aa22", "main", mreStageFile), "RAISE")
	writeABITestFile(t, filepath.Join(work, "9f", "77beef00", ".exitcode"), "1")

	got, err := nextflowFailedOutcome(work, []failedAttempt{
		{Hash: "4a/5cab94", Exit: 42},
		{Hash: "1b/0000aa", Exit: 1},
		{Hash: "1c/0000aa", Exit: 1},
		{Hash: "9f/77beef", Exit: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := runOutcome{Failed: true, Failures: []stageFailure{
		{Kind: failError, Stage: ""},
		{Kind: failAssert, Stage: "JOIN_BOOM"},
		{Kind: failError, Stage: "RAISE"},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("outcome (-want +got):\n%s", diff)
	}
}

// TestNextflowFailedOutcomeUnknownTask checks a trace hash with no work dir is
// an error, not a failure silently blaming no stage.
func TestNextflowFailedOutcomeUnknownTask(t *testing.T) {
	_, err := nextflowFailedOutcome(t.TempDir(), []failedAttempt{{Hash: "aa/bbbbbb", Exit: 1}})
	if !errors.Is(err, errABIMeta) {
		t.Errorf("err = %v, want errABIMeta", err)
	}
}

// outcomeTestAllowlist holds an outcome entry recording a failure that blames
// no stage, one recording an ASSERT in BOOM, and a comparator entry.
func outcomeTestAllowlist(t *testing.T) divergence.Allowlist {
	t.Helper()

	const entry = `{"id": %q, "kind": "outcome", "feature": "f", "summary": "s", "justification": "j",
		"evidence": ["TestMrpDiffFailureParity"], %s}`

	allow, err := divergence.Parse([]byte(`{"divergences": [` +
		fmt.Sprintf(entry, "nf-fails", `"outcome": {"failed": true}`) + "," +
		fmt.Sprintf(entry, "nf-asserts", `"outcome": {"failed": true, "failures": [{"kind": "assert", "stage": "BOOM"}]}`) + "," +
		fmt.Sprintf(entry, "by-stage", `"comparator": {"rule": "compare-failure-stage-and-kind"}`) + "]}"))
	if err != nil {
		t.Fatal(err)
	}

	return allow
}

func TestMrpOutcomeCaseValidate(t *testing.T) {
	boom := failedIn(failAssert, "BOOM")
	allow := outcomeTestAllowlist(t)

	cases := []struct {
		name    string
		tc      mrpOutcomeCase
		want    *outcomeDivergence
		wantErr bool
	}{
		{"mrp fails", mrpOutcomeCase{name: "f", mrp: boom}, nil, false},
		{"mrp succeeds without a divergence", mrpOutcomeCase{name: "f"}, nil, true},
		{"mrp succeeds with a divergence", mrpOutcomeCase{name: "f", divergence: "nf-fails"}, &outcomeDivergence{id: "nf-fails", nf: runOutcome{Failed: true}}, false},
		{"divergence not in the allowlist", mrpOutcomeCase{name: "f", mrp: boom, divergence: "gone"}, nil, true},
		{"divergence with no outcome matcher", mrpOutcomeCase{name: "f", mrp: boom, divergence: "by-stage"}, nil, true},
		{"divergence equal to mrp", mrpOutcomeCase{name: "f", mrp: boom, divergence: "nf-asserts"}, nil, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.tc.validate(allow)
			if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, errOutcomeCase)) {
				t.Errorf("validate() = %v, want an error: %v", err, c.wantErr)
			}

			if diff := cmp.Diff(c.want, got, cmp.AllowUnexported(outcomeDivergence{})); diff != "" {
				t.Errorf("divergence (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOutcomeRulesNeedTheirEntries feeds the outcome comparator rules the
// attempts of a stage_raise run: the checked-in allowlist excuses the
// stage-and-kind comparison, and removing its entry fails the case. RAISE is
// a stage exception, which mrp does not retry, so a run that attempted it
// twice fails the case, whatever the allowlist.
func TestOutcomeRulesNeedTheirEntries(t *testing.T) {
	once := []string{"PLAN", "N0_RAISE (SR.RAISE)"}
	retried := []string{"PLAN", "N0_RAISE (SR.RAISE)", "N0_RAISE (SR.RAISE)"}
	allow := mustAllowlist(t)

	if got := outcomeRuleReports(allow, "stage_raise", once); len(got) != 0 {
		t.Errorf("the checked-in allowlist leaves reports: %q", got)
	}

	cases := []struct {
		name     string
		allow    divergence.Allowlist
		attempts []string
		want     string
	}{
		{"stage_entry_removed", allow.Without("failure-report-details"), once, "reads only each failure's stage and kind"},
		{"retried", allow, retried, "attempted N0_RAISE (SR.RAISE) more than once"},
	}

	for _, c := range cases {
		got := outcomeRuleReports(c.allow, "stage_raise", c.attempts)
		if len(got) != 1 || !strings.Contains(got[0], c.want) {
			t.Errorf("%s: reports %q, want one containing %q", c.name, got, c.want)
		}
	}
}

func TestMrpOutcomeCaseCheck(t *testing.T) {
	boom := failedIn(failAssert, "BOOM")
	asError := failedIn(failError, "BOOM")
	div := &outcomeDivergence{id: "nf-succeeds", nf: runOutcome{}}

	cases := []struct {
		name string
		div  *outcomeDivergence
		mrp  runOutcome
		nf   runOutcome
		want []string
	}{
		{"parity", nil, boom, boom, nil},
		{"mro2nf succeeds", nil, boom, runOutcome{}, []string{"mro2nf outcome is success"}},
		{"kind differs", nil, boom, asError, []string{"mro2nf outcome is failure: error in BOOM"}},
		{"mrp changed", nil, asError, boom, []string{"mrp outcome is failure: error in BOOM"}},
		{"divergence holds", div, boom, runOutcome{}, nil},
		{"divergence gone", div, boom, boom, []string{"divergence nf-succeeds is gone"}},
		{"divergence changed", div, boom, asError, []string{"divergence nf-succeeds records success"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mrpOutcomeCase{mrp: boom}.check(c.div, c.mrp, c.nf)
			if len(got) != len(c.want) {
				t.Fatalf("check() = %q, want %d report(s) containing %q", got, len(c.want), c.want)
			}

			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("report %q does not contain %q", got[i], w)
				}
			}
		})
	}
}
