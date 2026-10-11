package e2e

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// genFinding is one way a run diverged from mrp, under one leg. Its key names
// what diverged (the outcome, a published output, a stage phase's ABI) so a
// shrink can require the same divergence to persist.
type genFinding struct {
	leg    string
	key    string
	detail string
}

func (f genFinding) id() string { return f.leg + " " + f.key }

// genVerdict is what running one program found. A program with a skip reason
// is no test: mrp rejects it before any stage runs, or mro2nf rejects it as a
// documented unsupported construct.
type genVerdict struct {
	skip     string
	findings []genFinding
	notes    []string
}

func (v genVerdict) String() string {
	var b strings.Builder

	for _, f := range v.findings {
		fmt.Fprintf(&b, "  [%s] %s\n    %s\n", f.leg, f.key, strings.ReplaceAll(strings.TrimRight(f.detail, "\n"), "\n", "\n    "))
	}

	return b.String()
}

func (v genVerdict) sharesFinding(ids map[string]bool) bool {
	return slices.ContainsFunc(v.findings, func(f genFinding) bool { return ids[f.id()] })
}

// shrinkTarget is the findings a shrink keeps one of: those keep matches,
// when it matches any; else a different outcome when there is one, else stage
// ABI differences, which locate where a divergence starts, else any. Keeping
// the most upstream evidence stops the shrink from trading the divergence for
// a downstream one it caused.
func shrinkTarget(v genVerdict, keep *regexp.Regexp) map[string]bool {
	if keep != nil {
		want := map[string]bool{}

		for _, f := range v.findings {
			if keep.MatchString(f.id()) {
				want[f.id()] = true
			}
		}

		if len(want) > 0 {
			return want
		}
	}

	classes := []func(key string) bool{
		func(key string) bool { return key == "outcome" },
		func(key string) bool { return strings.HasPrefix(key, "abi:") },
		func(string) bool { return true },
	}

	for _, inClass := range classes {
		want := map[string]bool{}

		for _, f := range v.findings {
			if inClass(f.key) {
				want[f.id()] = true
			}
		}

		if len(want) > 0 {
			return want
		}
	}

	return nil
}
