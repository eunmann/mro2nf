package cg

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/martian-lang/martian/martian/syntax"
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
// map call of known length, outside that call's forks, gathers the
// element's own forks: mrp constrains the merge to the outer fork its
// reference fixes (core/resolve.go ForkId.constrain). Each want is the outs
// of upstream mrp's (50ad6c0) run of the fixture: an empty outer fork
// merges to [], and outer forks of different widths each gather their own.
func TestMergeAcrossOuterForks(t *testing.T) {
	nested := func(t *testing.T) (*fixture, []string) {
		t.Helper()

		f := loadFixture(t, "map_pipe_nested")
		inner := func(i int) Part { return f.part("OUTER.INNER", ArrayIndex(i)) }
		dbl := func(i int) Part { return f.part("OUTER.INNER.DBL", ArrayIndex(i)) }

		return f, []string{
			f.outs("OUTER.INNER.DBL", Fork{inner(0), dbl(0)}, `{"y":2}`),
			f.outs("OUTER.INNER.DBL", Fork{inner(0), dbl(1)}, `{"y":4}`),
			f.outs("OUTER.INNER.DBL", Fork{inner(1), dbl(0)}, `{"y":6}`),
		}
	}

	sub := func(load func(*testing.T) *fixture, widths ...int) func(*testing.T) (*fixture, []string) {
		return func(t *testing.T) (*fixture, []string) {
			t.Helper()

			f := load(t)

			return f, subOuts(f, widths)
		}
	}

	cases := []struct {
		name string
		load func(*testing.T) (*fixture, []string)
		want string
	}{
		{"merge_across_outer", sub(fixtureNamed("merge_across_outer"), 3, 3), `{"subs":[{"ys":[1,4,9]},{"ys":[1,4,9]}]}`},
		{"merge_across_empty", sub(fixtureNamed("merge_across_empty"), 2, 0), `{"subs":[{"ys":[1,4]},{"ys":[]}]}`},
		{"merge_across_short", sub(fixtureNamed("merge_across_short"), 2, 1), `{"subs":[{"ys":[1,4]},{"ys":[1]}]}`},
		{"one_outer_fork", sub(oneOuterFork, 2), `{"subs":[{"ys":[1,4]}]}`},
		{"map_pipe_nested", nested, `{"yss":[[2,4],[6]]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, in := tc.load(t)

			raw, err := f.writeOuts(in...)
			if err != nil {
				t.Fatal(err)
			}

			wantArgs(t, "TOP outs", raw, tc.want)
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

// TestMergeAcrossOuterForksOfEveryShape: the element's own forks are what
// upstream mrp (50ad6c0) gathers too for a merge of a map call over a map,
// a merge across two outer map calls of several forks each, and a merged
// value other than a reference, at an empty fork as []. Each want is that
// mrp's outs of the program, with MK returning xs 1..n and km {a: 1, b: 2},
// and SQ its x squared.
func TestMergeAcrossOuterForksOfEveryShape(t *testing.T) {
	cases := []struct {
		name string
		src  string
		in   func(*fixture) []string
		want string
	}{
		{"map_call", acrossStages + `
pipeline SUB(in int n, out map<int> ys,)
{
    call MK(n = self.n,)
    map call SQ(x = split MK.km,)
    return (ys = SQ.y,)
}
`, mapCallOuts, `{"subs":[{"ys":{"a":1,"b":4}},{"ys":{"a":1,"b":4}}]}`},
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
`, twoOuterOuts, `{"subs":[{"s2":[{"ys":[1,4]},{"ys":[1,4]}]},{"s2":[{"ys":[1,4]},{"ys":[1,4]}]}]}`},
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
`, structOuts, `{"subs":[{"ps":[{"k":5,"y":1},{"k":5,"y":4}]},{"ps":[]}]}`},
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

			raw, err := f.writeOuts(tc.in(f)...)
			if err != nil {
				t.Fatal(err)
			}

			wantArgs(t, "TOP outs", raw, tc.want)
		})
	}
}

// mapCallOuts stages the outs of map_call's stages: at each outer fork, MK
// returns km {a: 1, b: 2} and SQ its square at each key.
func mapCallOuts(f *fixture) []string {
	in := make([]string, 0, 6)

	for s := range 2 {
		sub := f.part("TOP.SUB", ArrayIndex(s))

		for k, y := range map[string]string{"a": "1", "b": "4"} {
			in = append(in, f.outs("TOP.SUB.SQ", Fork{sub, f.part("TOP.SUB.SQ", MapKey(k))}, `{"y":`+y+`}`))
		}

		in = append(in, f.outs("TOP.SUB.MK", Fork{sub}, `{"xs":[],"km":{"a":1,"b":2}}`))
	}

	return in
}

// twoOuterOuts stages the outs of two_outer_calls' stages: two SQ forks,
// of 1 and 4, under each of SUB2's two forks under each of SUB's two.
func twoOuterOuts(f *fixture) []string {
	in := make([]string, 0, 12)

	for s := range 2 {
		sub := f.part("TOP.SUB", ArrayIndex(s))

		for s2 := range 2 {
			sub2 := f.part("TOP.SUB.SUB2", ArrayIndex(s2))

			for q := range 2 {
				in = append(in, f.outs("TOP.SUB.SUB2.SQ", Fork{sub, sub2, f.part("TOP.SUB.SUB2.SQ", ArrayIndex(q))}, `{"y":`+strconv.Itoa((q+1)*(q+1))+`}`))
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

// TestUnconstrainedMergeAcrossForksFails: a merge that fixes an outer
// fork's index with no constraint to gather that fork's forks alone, which
// mrp would gather across every outer fork, fails rather than gathering
// what mro2nf cannot check against mrp. merge_across_outer's merge, its
// constraint removed, is one.
func TestUnconstrainedMergeAcrossForksFails(t *testing.T) {
	f := loadFixture(t, "merge_across_outer")

	var merge *syntax.MergeExp

	walkMerges(f.g.root.ResolvedOutputs().Exp, func(m *syntax.MergeExp) {
		if m.Call.Fqid == "TOP.SUB.SQ" {
			merge = m
		}
	})

	if merge == nil || mergeConstraint(merge) == nil {
		t.Fatalf("merge_across_outer's outs hold no constrained merge of TOP.SUB.SQ")
	}

	free := *merge
	free.ForkNode = nil
	free.Value = &syntax.ArrayExp{Value: []syntax.Exp{merge.Value}}

	r := &resolver{g: f.g, st: emptyStore{}}
	if _, err := r.mergeIndices(&free, nil); !errors.Is(err, errMergeAcrossForks) {
		t.Errorf("mergeIndices = %v, want errMergeAcrossForks", err)
	}
}

// walkMerges calls visit on every merge in e.
func walkMerges(e syntax.Exp, visit func(*syntax.MergeExp)) {
	switch x := e.(type) {
	case *syntax.MergeExp:
		visit(x)
		walkMerges(x.Value, visit)
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			walkMerges(v, visit)
		}
	case *syntax.MapExp:
		for _, v := range x.Value {
			walkMerges(v, visit)
		}
	case *syntax.SplitExp:
		walkMerges(x.Value, visit)
	case *syntax.DisabledExp:
		walkMerges(x.Value, visit)
	}
}
