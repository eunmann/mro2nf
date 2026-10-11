package gen

import (
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// bindsBigFloat stands in for a divergence: it holds while the program binds
// a float literal of at least 1e6 somewhere.
func bindsBigFloat(p *Program) bool { return Features(p)["value:float-at-least-1e6"] }

func seedWith(t *testing.T, keep func(*Program) bool) *Program {
	t.Helper()

	for seed := uint64(1); seed <= 200; seed++ {
		if p := mustGenerate(t, seed); keep(p) && len(p.Stages) >= 3 {
			return p
		}
	}

	t.Fatal("no seed in 1..200 has the property")

	return nil
}

// TestShrinkFindsAMinimalProgram checks the shrink keeps the property, stays
// a valid program, and cuts a multi-stage program down to a call or two (a
// value passed through a sub pipeline keeps its two calls: the shrinker
// drops, it does not inline).
func TestShrinkFindsAMinimalProgram(t *testing.T) {
	p := seedWith(t, bindsBigFloat)

	small, evals := Shrink(p, bindsBigFloat, ShrinkOptions{MaxEvals: 2000, Parallel: 3})

	if !bindsBigFloat(small) {
		t.Fatalf("the shrunk program lost the property:\n%s", small.MRO())
	}

	if err := Validate(small); err != nil {
		t.Fatalf("the shrunk program does not compile: %v\n%s", err, small.MRO())
	}

	calls := 0
	for _, pl := range small.Pipelines {
		calls += len(pl.Calls)
	}

	if calls > 2 || len(small.Stages) > 1 {
		t.Errorf("shrunk %s to %s in %d evaluations, want at most two calls and a stage:\n%s",
			Size(p), Size(small), evals, small.MRO())
	}

	if !small.Shrunk {
		t.Error("the shrunk program is not marked shrunk")
	}
}

// TestShrinkIsDeterministic checks evaluating candidates in parallel picks
// the same program as evaluating them one at a time.
func TestShrinkIsDeterministic(t *testing.T) {
	p := seedWith(t, bindsBigFloat)

	serial, _ := Shrink(p, bindsBigFloat, ShrinkOptions{MaxEvals: 2000, Parallel: 1})
	parallel, _ := Shrink(p, bindsBigFloat, ShrinkOptions{MaxEvals: 2000, Parallel: 4})

	if diff := cmp.Diff(serial.MRO(), parallel.MRO()); diff != "" {
		t.Errorf("parallel shrink differs (-serial +parallel):\n%s", diff)
	}
}

// TestShrinkRespectsTheBudget checks the predicate runs at most MaxEvals
// times, not at all on a zero budget, and that a program no reduction was
// kept for is not marked shrunk.
func TestShrinkRespectsTheBudget(t *testing.T) {
	p := seedWith(t, bindsBigFloat)

	for _, budget := range []int{0, 1, 5} {
		var calls atomic.Int64

		always := func(*Program) bool {
			calls.Add(1)

			return true
		}

		small, evals := Shrink(p, always, ShrinkOptions{MaxEvals: budget, Parallel: 3})
		if evals > budget || int(calls.Load()) != evals {
			t.Errorf("budget %d: reported %d evaluations, ran %d", budget, evals, calls.Load())
		}

		if small.Shrunk != (evals > 0) {
			t.Errorf("budget %d: shrunk mark %t after %d evaluations that all kept a reduction", budget, small.Shrunk, evals)
		}
	}
}

// TestShrinkCandidatesAreValid checks every reduction of a sample of
// programs compiles and differs from its source.
func TestShrinkCandidatesAreValid(t *testing.T) {
	for seed := uint64(1); seed <= 10; seed++ {
		p := mustGenerate(t, seed)

		for _, c := range candidates(p) {
			if err := Validate(c); err != nil {
				t.Fatalf("seed %d: invalid candidate: %v", seed, err)
			}

			if c.MRO() == p.MRO() {
				t.Fatalf("seed %d: a candidate equals its source", seed)
			}
		}
	}
}
