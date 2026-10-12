package cg

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// nestedPlan is map_pipe_nested's call over n outer forks of one or two
// inner forks, alternately, compiled, and its witness's topology. The inner
// fork sets differ in length, so each outer fork has its own.
func nestedPlan(tb testing.TB, n int) (*Graph, Topology) {
	tb.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "map_pipe_nested", "pipeline.mro"))
	if err != nil {
		tb.Fatal(err)
	}

	decls, _, _ := strings.Cut(string(raw), "call OUTER(")

	sets := make([]string, n)
	for i := range sets {
		sets[i] = "[" + strconv.Itoa(2*i) + "]"
		if i%2 == 1 {
			sets[i] = "[" + strconv.Itoa(2*i) + ", " + strconv.Itoa(2*i+1) + "]"
		}
	}

	mro := filepath.Join(tb.TempDir(), "pipeline.mro")
	if err := os.WriteFile(mro, []byte(decls+"call OUTER(\n    sets = ["+strings.Join(sets, ", ")+"],\n)\n"), 0o644); err != nil {
		tb.Fatal(err)
	}

	src, err := ReadSource(mro, nil)
	if err != nil {
		tb.Fatal(err)
	}

	wg, err := src.Witness(nil)
	if err != nil {
		tb.Fatal(err)
	}

	wt, err := wg.Topology()
	if err != nil {
		tb.Fatal(err)
	}

	g, err := Load(mro, "", nil)
	if err != nil {
		tb.Fatal(err)
	}

	return g, wt
}

// nestedOwn is the entry fork elements of nestedPlan's roots.
func nestedOwn(tb testing.TB, g *Graph, wt Topology) map[string][]Element {
	tb.Helper()

	dir := tb.TempDir()
	if err := g.PostEntry(dir); err != nil {
		tb.Fatal(err)
	}

	own := map[string][]Element{}

	for _, r := range wt.Roots {
		els, err := g.readElements(filepath.Join(dir, r.Fqid))
		if err != nil {
			tb.Fatal(err)
		}

		own[r.Fqid] = els
	}

	return own
}

// TestAgreeIndexMatchesScan: the index finds, in order, exactly the items a
// scan finds agreeing with each query, over items and queries that name
// different roots, by part and by ordinal.
func TestAgreeIndexMatchesScan(t *testing.T) {
	g, _ := nestedPlan(t, 4)

	inner, err := g.forkRoot("OUTER.INNER")
	if err != nil {
		t.Fatal(err)
	}

	dbl, err := g.forkRoot("OUTER.INNER.DBL")
	if err != nil {
		t.Fatal(err)
	}

	items := make([]agreeItem, 0, 21)

	for i := range 3 {
		items = append(items, agreeItem{fork: Fork{{Root: inner, Index: ArrayIndex(i)}}})

		for j := range 2 {
			items = append(items,
				agreeItem{fork: Fork{{Root: inner, Index: ArrayIndex(i)}, {Root: dbl, Index: ArrayIndex(j)}}},
				agreeItem{ords: map[string]int{inner.Fqid: i, dbl.Fqid: j}, fork: Fork{{Root: dbl, Index: ArrayIndex(j)}}},
				agreeItem{ords: map[string]int{inner.Fqid: i}})
		}
	}

	x := newAgreeIndex(items)

	for _, q := range append(slices.Clone(items), agreeItem{}, agreeItem{fork: Fork{{Root: dbl, Index: ArrayIndex(5)}}}) {
		var want []int

		for i, it := range items {
			if agrees(it.fork, q.fork) && ordsMatch(it.ords, q.ords) {
				want = append(want, i)
			}
		}

		if diff := cmp.Diff(want, x.agreeing(q.ords, q.fork)); diff != "" {
			t.Errorf("agreeing %v %s (-scan +index):\n%s", q.ords, q.fork, diff)
		}
	}
}

// TestPlanEntryLinear: planning a nested map call's entry forks costs about
// the same per fork at two outer widths 8-fold apart. Planning that scans
// the forks of a root under each fork of its outer roots costs 5x more per
// fork at the wider; the bound leaves room for timing noise, and each
// width's time is the fastest of interleaved runs.
func TestPlanEntryLinear(t *testing.T) {
	widths := []int{512, 4096}
	owns := make([]map[string][]Element, len(widths))
	roots := make([][]TopoRoot, len(widths))
	best := make([]float64, len(widths))

	for i, n := range widths {
		g, wt := nestedPlan(t, n)
		owns[i], roots[i], best[i] = nestedOwn(t, g, wt), wt.Roots, math.Inf(1)
	}

	for range 5 {
		for i := range widths {
			start := time.Now()
			if _, _, err := planEntry(roots[i], owns[i]); err != nil {
				t.Fatal(err)
			}

			best[i] = min(best[i], float64(time.Since(start).Nanoseconds())/float64(forkCount(owns[i])))
		}
	}

	if best[1] > 2.5*best[0] {
		t.Errorf("plan: %.0f ns/fork at %d outer forks, %.0f at %d, want about the same", best[0], widths[0], best[1], widths[1])
	}
}

func forkCount(own map[string][]Element) int {
	n := 0
	for _, els := range own {
		n += len(els)
	}

	return n
}

func ordsMatch(a, b map[string]int) bool {
	for k, v := range a {
		if w, ok := b[k]; ok && w != v {
			return false
		}
	}

	return true
}

// BenchmarkPlanEntry plans the entry forks of a nested map call at two
// outer widths, 8-fold apart; ns/fork, the time per planned fork, stays
// flat with the width when planning is linear in the forks.
func BenchmarkPlanEntry(b *testing.B) {
	for _, n := range []int{1024, 8192} {
		b.Run("outer="+strconv.Itoa(n), func(b *testing.B) {
			g, wt := nestedPlan(b, n)
			own := nestedOwn(b, g, wt)

			for b.Loop() {
				if _, _, err := planEntry(wt.Roots, own); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*forkCount(own)), "ns/fork")
		})
	}
}
