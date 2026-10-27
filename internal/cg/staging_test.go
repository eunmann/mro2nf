package cg

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/martian-lang/martian/martian/syntax"
)

// standInSkipped are the fixtures and corpus entries the stand-in cannot
// run, by why.
func standInSkipped() map[string]string {
	return map[string]string{
		"split_gate_entry": "it zips entry collections with SRC's outs, whose keys and lengths no stand-in knows",
		"forkdisabled":     "GATED zips the entry's xs with FLAGS's skips, whose length no stand-in knows",
		"post_outer_fork":  "SRC's post fails, as mrp does (TestPostFailsUnderAnOuterForkItDoesNotHold)",
		"witness_known_inner": "its witness's rows differ in length, where ROW's post fails as mrp's run panics " +
			"(map filtering an array); its literal is TestMrpDiff's",
	}
}

// TestStandInStagesOnlyWhatItReads holds every instance of every fixture to
// mrp's data movement: an mrp job reads, of each node its bindings read,
// the outs of the forks its resolution reads, so an instance may be given
// no more instances of a node than its resolution reads, or one where it
// reads none, the instance a value the gate holds disabled would read. It
// runs each fixture's literal invocation and its witness's, whose
// collections all fork, and of different lengths where nested.
func TestStandInStagesOnlyWhatItReads(t *testing.T) {
	var found []string

	for _, e := range sweepEntries(t) {
		if _, skip := standInSkipped()[e.name]; skip {
			continue
		}

		src, err := ReadSource(e.mro, e.mroPath)
		if err != nil {
			t.Fatalf("%s: %v", e.label, err)
		}

		witness, _, err := src.Witness(e.mroPath)
		if err != nil {
			t.Fatalf("%s: witness: %v", e.label, err)
		}

		literal, err := Load(e.mro, "", e.mroPath)
		if err != nil {
			t.Fatalf("%s: %v", e.label, err)
		}

		for kind, g := range map[string]*Graph{"literal": literal, "witness": witness} {
			name := e.label + " " + kind

			t.Run(name, func(t *testing.T) {
				s := standInOf(t, g)
				s.audit = func(a stagingAudit) {
					st, err := g.Stage(a.node.Fqid)
					if err != nil {
						t.Fatal(err)
					}

					found = append(found, overstaged(name, a, gatedValueReads(st))...)
				}

				s.stages()
			})
		}
	}

	slices.Sort(found)

	for _, f := range found {
		t.Error(f)
	}
}

// overstaged are the reads of the audited instance that give it more
// instances of a node than its resolution reads, or, of a node gated
// reads, than one where it reads none: a disable-wrapped value whose gate
// holds it off at run time reads none of the one instance the driver gives
// it. Any other node it is given and reads none of is a read only the
// topology makes, as counting a merge's forks under a split aligned to
// them was (#614): the instance's _args hold none of it.
func overstaged(name string, a stagingAudit, gated map[string]bool) []string {
	var out []string

	for _, node := range slices.Sorted(maps.Keys(a.given)) {
		allowed := a.resolved[node]
		if gated[node] {
			allowed = max(1, allowed)
		}

		if a.given[node] > allowed {
			out = append(out, fmt.Sprintf("%s: %s at %s is given %d instances of %s and reads %d", name, a.node.Fqid, a.fork, a.given[node], node, a.resolved[node]))
		}
	}

	return out
}

// gatedValueReads are the nodes st's bindings read within a disable-wrapped
// value.
func gatedValueReads(st *syntax.CallGraphStage) map[string]bool {
	out := map[string]bool{}

	var walk func(e syntax.Exp)

	walk = func(e syntax.Exp) {
		switch x := e.(type) {
		case *syntax.DisabledExp:
			for _, r := range x.Value.FindRefs() {
				out[r.Id] = true
			}

			walk(x.Value)
		case *syntax.SplitExp:
			walk(x.Value)
		case *syntax.MergeExp:
			walk(x.Value)
		case *syntax.ArrayExp:
			for _, v := range x.Value {
				walk(v)
			}
		case *syntax.MapExp:
			for _, v := range x.Value {
				walk(v)
			}
		}
	}

	for _, e := range bindings(st) {
		walk(e)
	}

	return out
}
