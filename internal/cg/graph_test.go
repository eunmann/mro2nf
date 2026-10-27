package cg

import (
	"os"
	"testing"

	"github.com/martian-lang/martian/martian/syntax"
)

// TestSplitCallsAreMartians holds splitCalls to Martian's unexported
// findSplitCalls(…, false), from which the compiler forks a stage on each
// of the map calls around it its inputs and disable bindings split
// (syntax/resolve_stage.go:322 resolveForks): over every fixture, the
// calls splitCalls finds in a stage's bindings that are any fork root's
// are exactly the calls of the stage's fork roots.
func TestSplitCallsAreMartians(t *testing.T) {
	checked := 0

	for _, e := range sweepEntries(t) {
		raw, err := os.ReadFile(e.mro)
		if err != nil {
			t.Fatal(err)
		}

		g, err := Compile(raw, e.mro, e.mroPath)
		if err != nil {
			continue
		}

		rootCalls := map[*syntax.CallStm]bool{}
		for _, r := range g.roots {
			rootCalls[r.Call()] = true
		}

		for fqid, st := range g.stages {
			if d := splitCallsDiff(st, rootCalls); d != "" {
				t.Errorf("%s %s: %s", e.label, fqid, d)
			}
		}

		checked++
	}

	if checked < 100 {
		t.Errorf("checked %d invocations, want every fixture's", checked)
	}
}

// splitCallsDiff is how the root calls splitCalls finds in st's bindings
// differ from the calls of st's fork roots, or "" where they agree.
func splitCallsDiff(st *syntax.CallGraphStage, rootCalls map[*syntax.CallStm]bool) string {
	found := map[*syntax.CallStm]bool{}
	for _, b := range st.ResolvedInputs() {
		splitCalls(b.Exp, found)
	}

	for _, d := range st.Disabled() {
		splitCalls(d, found)
	}

	forked := map[*syntax.CallStm]bool{}
	for _, r := range st.ForkRoots() {
		forked[r.Call()] = true

		if !found[r.Call()] && !staticallyDisabled(st) {
			return "forked on " + r.Fqid + ", which splitCalls does not find"
		}
	}

	for c := range found {
		if rootCalls[c] && !forked[c] {
			return "splitCalls finds " + c.Id + ", which it is not forked on"
		}
	}

	return ""
}

// staticallyDisabled reports whether the compiler disabled st outright,
// which then resolves its inputs to null after forking it
// (syntax/resolve_stage.go:165 alwaysDisable).
func staticallyDisabled(st *syntax.CallGraphStage) bool {
	d := st.Disabled()
	if len(d) == 0 {
		return false
	}

	b, ok := d[0].(*syntax.BoolExp)

	return ok && b.Value
}
