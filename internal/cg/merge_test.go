package cg

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
)

// subOuts stages the outs of TOP.SUB's stages in the merge_across fixtures:
// at outer fork s, MK returns xs 1..widths[s] and SQ at inner fork q
// returns (q+1)².
func subOuts(f *fixture, widths []int) []string {
	f.t.Helper()

	var in []string

	for s, w := range widths {
		sub := f.part("TOP.SUB", ArrayIndex(s))
		xs := make([]string, w)

		for q := range w {
			xs[q] = strconv.Itoa(q + 1)
			sq := f.part("TOP.SUB.SQ", ArrayIndex(q))
			in = append(in, f.outs("TOP.SUB.SQ", Fork{sub, sq}, `{"y":`+strconv.Itoa((q+1)*(q+1))+`}`))
		}

		in = append(in, f.outs("TOP.SUB.MK", Fork{sub}, `{"xs":[`+strings.Join(xs, ",")+`]}`))
	}

	return in
}

func fixtureNamed(name string) func(*testing.T) *fixture {
	return func(t *testing.T) *fixture {
		t.Helper()

		return loadFixture(t, name)
	}
}

// oneOuterFork is merge_across_outer over one outer fork, gen-diff seed 5's
// shape.
func oneOuterFork(t *testing.T) *fixture {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "merge_across_outer", "pipeline.mro"))
	if err != nil {
		t.Fatal(err)
	}

	return loadSource(t, strings.Replace(string(raw), "split [3, 3]", "split [2]", 1))
}

// TestMergeAcrossOuterForks: a merge Martian expanded per fork of an outer
// map call of known length, outside that call's forks, gathers what mrp
// gathers: every inner fork across the outer forks, in mrp's fork order,
// each read at the element's own outer fork, an empty fork as null. Each
// want is mrp's outs of the fixture; over one outer fork, each element
// gathers its own forks, as mrp's run of gen-diff seed 5 does.
func TestMergeAcrossOuterForks(t *testing.T) {
	cases := []struct {
		name   string
		load   func(*testing.T) *fixture
		widths []int
		want   string
	}{
		{"merge_across_outer", fixtureNamed("merge_across_outer"), []int{3, 3}, `{"subs":[{"ys":[1,1,4,9,4,9]},{"ys":[1,1,4,9,4,9]}]}`},
		{"merge_across_empty", fixtureNamed("merge_across_empty"), []int{2, 0}, `{"subs":[{"ys":[1,null,4]},{"ys":[null,null,null]}]}`},
		{"one_outer_fork", oneOuterFork, []int{2}, `{"subs":[{"ys":[1,4]}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.load(t)

			out := f.next("pipeline")
			if err := f.g.WriteOuts(OutsSpec{In: subOuts(f, tc.widths), Out: out}); err != nil {
				t.Fatal(err)
			}

			raw, err := shim.ReadBundle(out)
			if err != nil {
				t.Fatal(err)
			}

			wantArgs(t, "TOP outs", string(raw), tc.want)
		})
	}
}

// TestMergeAcrossOuterForksFails: where an element's own outer fork lacks
// an inner fork another outer fork has, the merge fails as mrp's run of the
// fixture fails resolving the pipeline outs ("could not match reference to
// a specific fork"): an inner map call over a stage output
// (merge_across_short) or over a literal whose elements differ in length
// (map_pipe_nested).
func TestMergeAcrossOuterForksFails(t *testing.T) {
	short := loadFixture(t, "merge_across_short")

	nested := loadFixture(t, "map_pipe_nested")
	inner := func(i int) Part { return nested.part("OUTER.INNER", ArrayIndex(i)) }
	dbl := func(i int) Part { return nested.part("OUTER.INNER.DBL", ArrayIndex(i)) }

	cases := []struct {
		name string
		f    *fixture
		in   []string
	}{
		{"merge_across_short", short, subOuts(short, []int{2, 1})},
		{"map_pipe_nested", nested, []string{
			nested.outs("OUTER.INNER.DBL", Fork{inner(0), dbl(0)}, `{"y":2}`),
			nested.outs("OUTER.INNER.DBL", Fork{inner(0), dbl(1)}, `{"y":4}`),
			nested.outs("OUTER.INNER.DBL", Fork{inner(1), dbl(0)}, `{"y":6}`),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.g.WriteOuts(OutsSpec{In: tc.in, Out: tc.f.next("pipeline")})
			if !errors.Is(err, errMergeUnmatched) {
				t.Fatalf("pipeline outs: %v, want %v", err, errMergeUnmatched)
			}
		})
	}
}
