//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errGenRunFailed stands in for a Nextflow run that exited non-zero.
var errGenRunFailed = errors.New("run exited 1")

// TestGenDiffPublishedAppliesEveryRule runs TestGenDiff's published-output
// comparison on publishedPair as a program that is no fixture, with the
// relative-paths entry tracked by an issue and scoped to pairFixture. The
// scoped rule excuses mrp's absolute paths for the program too; once mrp's
// paths agree the entry excuses nothing, which the comparison of the named
// fixture reports and that of a generated program does not; and a difference
// no entry excuses is still a finding.
func TestGenDiffPublishedAppliesEveryRule(t *testing.T) {
	const program = "fixture"

	allow := allowlistScoping(t, "outs-json-relative-paths", pairFixture)
	ps, results := publishedPair(t)
	mrpJSON := filepath.Join(ps, "P", "fork0", "_outs")
	nfOuts := filepath.Join(results, "pipeline_outs.json")

	relative, err := os.ReadFile(nfOuts)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, mrpOuts, nfOuts string
		want                  []string
	}{
		{"paths_absolute", "", string(relative), nil},
		{"paths_agree", string(relative), string(relative), nil},
		{
			"number_token", string(relative),
			`{"chained": "chained.txt", "linked": "linked.txt", "note": "note.txt", "stats": {"count": 4, "total": 10, "mean": 2.5}}`,
			[]string{"outs:stats"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.mrpOuts != "" {
				writeABITestFile(t, mrpJSON, c.mrpOuts)
			}

			writeABITestFile(t, nfOuts, c.nfOuts)

			got, err := genPublishedFindings(allow.RulesEverywhere(), "default", program, ps, results)
			if err != nil {
				t.Fatal(err)
			}

			if keys := findingKeys(got); keys != strings.Join(c.want, ",") {
				t.Errorf("findings %v, want keys %v", got, c.want)
			}
		})
	}

	writeABITestFile(t, mrpJSON, string(relative))
	writeABITestFile(t, nfOuts, string(relative))

	reports, err := diffPublished(allow, pairFixture, ps, results)
	if err != nil {
		t.Fatal(err)
	}

	if len(reports) != 1 || !strings.Contains(reports[0], "excuses nothing in "+pairFixture) {
		t.Errorf("the fixture comparison reports %q, want the relative-paths entry excusing nothing", reports)
	}
}

// TestGenDiffOutcomeAppliesEveryRule runs TestGenDiff's outcome comparison
// on a failed run of a program named like a fixture the retry entry is scoped
// to. The issue-tracked retry rule excuses a retried stage error, a run with
// no retry does not fail the program as excusing nothing, which it does for
// the named fixture, and an outcome no rule excuses is still a finding.
func TestGenDiffOutcomeAppliesEveryRule(t *testing.T) {
	const program = "stage_raise"

	env := genEnv{allow: genAllowlist(t)}
	runErr := fmt.Errorf("nextflow: %w", errGenRunFailed)
	retried := "1b/0000aa\tP:RAISE (1)\tFAILED\t1\n1c/0000aa\tP:RAISE (1)\tFAILED\t1\n"

	cases := []struct {
		name, trace string
		mrp         runOutcome
		want        string
	}{
		{"retried", retried, failedIn(failError, "RAISE"), ""},
		{"not_retried", "1b/0000aa\tP:RAISE (1)\tFAILED\t1\n", failedIn(failError, "RAISE"), ""},
		{"other_outcome", retried, failedIn(failAssert, "RAISE"), "outcome"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proj := t.TempDir()
			writeABITestFile(t, filepath.Join(proj, "trace.txt"), "hash\tname\tstatus\texit\n"+c.trace)
			writeABITestFile(t, filepath.Join(proj, "work", "1b", "0000aa11", "main", mreStageFile), "RAISE")
			writeABITestFile(t, filepath.Join(proj, "work", "1c", "0000aa22", "main", mreStageFile), "RAISE")

			v, err := outcomeLegVerdict(env, genLegs["default"], program, proj, c.mrp, runErr)
			if err != nil {
				t.Fatal(err)
			}

			if keys := findingKeys(v.findings); keys != c.want {
				t.Errorf("findings %v, want keys %q", v.findings, c.want)
			}
		})
	}

	if got := outcomeRuleReports(mustAllowlist(t), program, []string{"P:RAISE (1)"}); len(got) != 1 || !strings.Contains(got[0], "excuses nothing") {
		t.Errorf("the fixture comparison reports %q, want the retry entry excusing nothing", got)
	}
}

func findingKeys(findings []genFinding) string {
	keys := make([]string, 0, len(findings))
	for _, f := range findings {
		keys = append(keys, f.key)
	}

	return strings.Join(keys, ",")
}
