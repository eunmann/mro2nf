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

// acrossStages are merge_across_outer's stages, MK also returning its xs as
// a map.
const acrossStages = `
stage MK(
    in  int      n,
    out int[]    xs,
    out map<int> km,
    src py       "stages/mk",
)

stage SQ(
    in  int x,
    out int y,
    src py  "stages/sq",
)
`

// TestMergeAcrossOuterForksRefused: a merge across an outer map call's forks
// that mro2nf does not list as mrp does fails the outs, not gathering only
// the element's own forks: a merge of a map call, a merge across two outer
// map calls of several forks each, and a merged value other than a
// reference at an empty fork.
func TestMergeAcrossOuterForksRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
		in   func(*fixture) []string
	}{
		{"map_call", acrossStages + `
pipeline SUB(in int n, out map<int> ys,)
{
    call MK(n = self.n,)
    map call SQ(x = split MK.km,)
    return (ys = SQ.y,)
}
`, mapCallOuts},
		{"two_outer_calls", acrossStages + `
pipeline SUB2(in int n, out int[] ys,)
{
    call MK(n = self.n,)
    map call SQ(x = split MK.xs,)
    return (ys = SQ.y,)
}

pipeline SUB(in int n, out SUB2[] s2,)
{
    map call SUB2(n = split [self.n, self.n],)
    return (s2 = SUB2,)
}
`, twoOuterOuts},
		{"struct_at_empty_fork", acrossStages + `
pipeline P(in int x, out int y, out int k,)
{
    call SQ(x = self.x,)
    return (y = SQ.y, k = 5,)
}

pipeline SUB(in int n, out P[] ps,)
{
    call MK(n = self.n,)
    map call P(x = split MK.xs,)
    return (ps = P,)
}
`, structOuts},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := loadSource(t, tc.src+`
pipeline TOP(out SUB[] subs,)
{
    map call SUB(n = split [2, 2],)
    return (subs = SUB,)
}

call TOP()
`)

			err := f.g.WriteOuts(OutsSpec{In: tc.in(f), Out: f.next("pipeline")})
			if !errors.Is(err, errMergeAcrossForks) {
				t.Fatalf("pipeline outs: %v, want %v", err, errMergeAcrossForks)
			}
		})
	}
}

// mapCallOuts stages the outs of map_call's stages: at each outer fork, MK
// returns km {a, b} and SQ a y at each key.
func mapCallOuts(f *fixture) []string {
	in := make([]string, 0, 6)

	for s := range 2 {
		sub := f.part("TOP.SUB", ArrayIndex(s))

		for _, k := range []string{"a", "b"} {
			in = append(in, f.outs("TOP.SUB.SQ", Fork{sub, f.part("TOP.SUB.SQ", MapKey(k))}, `{"y":1}`))
		}

		in = append(in, f.outs("TOP.SUB.MK", Fork{sub}, `{"xs":[],"km":{"a":1,"b":2}}`))
	}

	return in
}

// twoOuterOuts stages the outs of two_outer_calls' stages: two SQ forks
// under each of SUB2's two forks under each of SUB's two.
func twoOuterOuts(f *fixture) []string {
	in := make([]string, 0, 12)

	for s := range 2 {
		sub := f.part("TOP.SUB", ArrayIndex(s))

		for s2 := range 2 {
			sub2 := f.part("TOP.SUB.SUB2", ArrayIndex(s2))

			for q := range 2 {
				in = append(in, f.outs("TOP.SUB.SUB2.SQ", Fork{sub, sub2, f.part("TOP.SUB.SUB2.SQ", ArrayIndex(q))}, `{"y":1}`))
			}

			in = append(in, f.outs("TOP.SUB.SUB2.MK", Fork{sub, sub2}, `{"xs":[1,2],"km":{}}`))
		}
	}

	return in
}

// structOuts stages the outs of struct_at_empty_fork's stages: P has two
// forks under SUB's first fork and none under its second.
func structOuts(f *fixture) []string {
	first := f.part("TOP.SUB", ArrayIndex(0))

	return []string{
		f.outs("TOP.SUB.P.SQ", Fork{first, f.part("TOP.SUB.P", ArrayIndex(0))}, `{"y":1}`),
		f.outs("TOP.SUB.P.SQ", Fork{first, f.part("TOP.SUB.P", ArrayIndex(1))}, `{"y":4}`),
		f.outs("TOP.SUB.MK", Fork{first}, `{"xs":[1,2],"km":{}}`),
		f.outs("TOP.SUB.MK", Fork{f.part("TOP.SUB", ArrayIndex(1))}, `{"xs":[],"km":{}}`),
	}
}
