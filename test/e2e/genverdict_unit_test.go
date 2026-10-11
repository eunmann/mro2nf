package e2e

import (
	"regexp"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestShrinkTargetPrefersUpstreamEvidence checks a shrink keeps the outcome
// when it differs, else the stage ABI differences over what they cause
// downstream (an outcome-rules report is no outcome), and that keep overrides
// the order only when it matches.
func TestShrinkTargetPrefersUpstreamEvidence(t *testing.T) {
	outcome := genFinding{leg: "nf", key: "outcome"}
	rules := genFinding{leg: "nf", key: "outcome-rules"}
	abi := genFinding{leg: "nf", key: "abi:P.A.fork0.main"}
	outs := genFinding{leg: "nf", key: "outs.json"}

	cases := []struct {
		name     string
		findings []genFinding
		keep     *regexp.Regexp
		want     map[string]bool
	}{
		{"outcome first", []genFinding{outs, abi, outcome}, nil, map[string]bool{outcome.id(): true}},
		{"abi over outcome-rules", []genFinding{rules, abi}, nil, map[string]bool{abi.id(): true}},
		{"any when no class matches", []genFinding{rules, outs}, nil, map[string]bool{rules.id(): true, outs.id(): true}},
		{"keep matches", []genFinding{outcome, outs}, regexp.MustCompile(`outs`), map[string]bool{outs.id(): true}},
		{"keep matches none", []genFinding{outcome, outs}, regexp.MustCompile(`nothing`), map[string]bool{outcome.id(): true}},
		{"no findings", nil, nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shrinkTarget(genVerdict{findings: tc.findings}, tc.keep)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("shrinkTarget (-want +got):\n%s", diff)
			}
		})
	}
}
