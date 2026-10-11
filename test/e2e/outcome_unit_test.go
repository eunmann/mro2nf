package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestMrpFailedOutcome checks a failure is attributed to the stage of the fork
// it was recorded in, whether mrp wrote it in a phase dir (stage code) or in
// the fork dir (mrp's own checks), and that _assert and _errors keep apart.
func TestMrpFailedOutcome(t *testing.T) {
	ps := t.TempDir()
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

func TestMrpOutcomeCaseValidate(t *testing.T) {
	boom := failedIn(failAssert, "BOOM")

	cases := []struct {
		name    string
		tc      mrpOutcomeCase
		wantErr bool
	}{
		{"mrp fails", mrpOutcomeCase{name: "f", mrp: boom}, false},
		{"mrp succeeds without a divergence", mrpOutcomeCase{name: "f"}, true},
		{"mrp succeeds with a divergence", mrpOutcomeCase{name: "f", divergence: &outcomeDivergence{issue: 1, nf: runOutcome{Failed: true}}}, false},
		{"divergence without an issue", mrpOutcomeCase{name: "f", mrp: boom, divergence: &outcomeDivergence{}}, true},
		{"divergence equal to mrp", mrpOutcomeCase{name: "f", mrp: boom, divergence: &outcomeDivergence{issue: 1, nf: boom}}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.tc.validate()
			if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, errOutcomeCase)) {
				t.Errorf("validate() = %v, want an error: %v", err, c.wantErr)
			}
		})
	}
}

func TestMrpOutcomeCaseCheck(t *testing.T) {
	boom := failedIn(failAssert, "BOOM")
	asError := failedIn(failError, "BOOM")
	div := &outcomeDivergence{issue: 275, nf: runOutcome{}}

	cases := []struct {
		name string
		tc   mrpOutcomeCase
		mrp  runOutcome
		nf   runOutcome
		want []string
	}{
		{"parity", mrpOutcomeCase{mrp: boom}, boom, boom, nil},
		{"mro2nf succeeds", mrpOutcomeCase{mrp: boom}, boom, runOutcome{}, []string{"mro2nf outcome is success"}},
		{"kind differs", mrpOutcomeCase{mrp: boom}, boom, asError, []string{"mro2nf outcome is failure: error in BOOM"}},
		{"mrp changed", mrpOutcomeCase{mrp: boom}, asError, boom, []string{"mrp outcome is failure: error in BOOM"}},
		{"divergence holds", mrpOutcomeCase{mrp: boom, divergence: div}, boom, runOutcome{}, nil},
		{"divergence gone", mrpOutcomeCase{mrp: boom, divergence: div}, boom, boom, []string{"divergence #275 is gone"}},
		{"divergence changed", mrpOutcomeCase{mrp: boom, divergence: div}, boom, asError, []string{"divergence #275 records success"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.tc.check(c.mrp, c.nf)
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
