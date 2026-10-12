package cg

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Each expected _args in this file is one a real mrp run of the source gave
// the named fork (mrp built at the Martian commit go.mod pins).

// indepOfOuterMro: SUB2's forks read only GEN, not OUTER2's fork, though
// ADD is also forked on OUTER2 and DEP is not.
const indepOfOuterMro = `
stage GEN(out int[] xs, src py "stages/gen",)
stage ECHO(in int v, out int r, src py "stages/echo",)
stage ADD(in int a, in int b, out int r, src py "stages/add",)

pipeline SUB2(in int x, in int b, out int r, out int s,)
{
    call ECHO as DEP(v = self.x,)
    call ADD(a = self.x, b = self.b,)
    return (r = DEP.r, s = ADD.r,)
}

pipeline OUTER2(in int o, in int[] xs, out SUB2[] subs,)
{
    map call SUB2(x = split self.xs, b = self.o,)
    return (subs = SUB2,)
}

pipeline TOP(out OUTER2[] outs,)
{
    call GEN()
    map call OUTER2(o = split [20, 30], xs = GEN.xs,)
    return (outs = OUTER2,)
}

call TOP()
`

// runtimeOuterMro: OUTER maps over GEN's outs; ECHO's forks are the
// elements of OUTER's element, and ADD's split is a literal.
const runtimeOuterMro = `
stage GEN(out int[][] xss, src py "stages/gen",)
stage ECHO(in int v, out int r, src py "stages/echo",)
stage ADD(in int a, in int[] b, out int r, src py "stages/add",)

pipeline OUTER(in int[] xs, out int[] rs, out int[] ss,)
{
    map call ECHO(v = split self.xs,)
    map call ADD(a = split [1, 2], b = self.xs,)
    return (rs = ECHO.r, ss = ADD.r,)
}

pipeline TOP(out OUTER[] outs,)
{
    call GEN()
    map call OUTER(xs = split GEN.xss,)
    return (outs = OUTER,)
}

call TOP()
`

func loadSource(t *testing.T, src string) *fixture {
	t.Helper()

	mro := filepath.Join(t.TempDir(), "pipeline.mro")
	if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{t: t, g: g, mro: mro, dir: t.TempDir()}
}

func baseNames(dirs []string) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = filepath.Base(d)
	}

	return out
}

// TestInnerForkIndependentOfOuter: one fork layout for SUB2 on every load,
// whichever of its stages is also forked on OUTER2 (mrp: DEP/fork<j> gets
// v = j+1, ADD/fork<i>_fork<j> gets a = j+1, b = 20+10i).
func TestInnerForkIndependentOfOuter(t *testing.T) {
	for range 30 {
		f := loadSource(t, indepOfOuterMro)

		entry := f.next("entry")
		if err := f.g.PostEntry(entry); err != nil {
			t.Fatal(err)
		}

		gen := f.outs("TOP.GEN", nil, `{"xs":[1,2]}`)
		subs := elementDirs(t, f.post(gen, gen), "TOP.OUTER2.SUB2")

		if d := cmp.Diff([]string{"0000", "0001"}, baseNames(subs)); d != "" {
			t.Fatalf("SUB2 fork names (-want +got):\n%s", d)
		}

		for i, o := range elementDirs(t, entry, "TOP.OUTER2") {
			for j, s := range subs {
				_, got := f.args("TOP.OUTER2.SUB2.ADD", o, s)
				wantArgs(t, fmt.Sprintf("ADD fork%d_fork%d", i, j), got, fmt.Sprintf(`{"a":%d,"b":%d}`, j+1, 20+10*i))
			}
		}

		for j, s := range subs {
			_, got := f.args("TOP.OUTER2.SUB2.DEP", s)
			wantArgs(t, fmt.Sprintf("DEP fork%d", j), got, fmt.Sprintf(`{"v":%d}`, j+1))
		}
	}
}

// TestNestedForkUnderRuntimeOuter: GEN's post enumerates OUTER's forks to
// write ECHO's, though GEN is not forked on OUTER, and the entry writes
// ADD's literal forks without OUTER's. Each instance stages an element of
// each of its roots (mrp: ECHO runs v = 1, 2 under OUTER's fork 0 and v = 3
// under fork 1; ADD runs every a in [1, 2] with every b).
func TestNestedForkUnderRuntimeOuter(t *testing.T) {
	f := loadSource(t, runtimeOuterMro)

	entry := f.next("entry")
	if err := f.g.PostEntry(entry); err != nil {
		t.Fatal(err)
	}

	gen := f.outs("TOP.GEN", nil, `{"xss":[[1,2],[3]]}`)
	forks := f.post(gen, gen)

	outers := elementDirs(t, forks, "TOP.OUTER")
	echoes := elementDirs(t, forks, "TOP.OUTER.ECHO")

	if d := cmp.Diff([]string{"0000.0000", "0000.0001", "0001.0000"}, baseNames(echoes)); d != "" {
		t.Fatalf("ECHO fork names (-want +got):\n%s", d)
	}

	underOuter := []int{0, 0, 1}

	for i, e := range echoes {
		_, got := f.args("TOP.OUTER.ECHO", outers[underOuter[i]], e)
		wantArgs(t, "ECHO "+filepath.Base(e), got, fmt.Sprintf(`{"v":%d}`, i+1))
	}

	adds := elementDirs(t, entry, "TOP.OUTER.ADD")
	if d := cmp.Diff([]string{"0000", "0001"}, baseNames(adds)); d != "" {
		t.Fatalf("ADD fork names (-want +got):\n%s", d)
	}

	xss := []string{"[1,2]", "[3]"}

	for i, o := range outers {
		for j, a := range adds {
			_, got := f.args("TOP.OUTER.ADD", o, a)
			wantArgs(t, fmt.Sprintf("ADD a=%d b=%s", j+1, xss[i]), got, fmt.Sprintf(`{"a":%d,"b":%s}`, j+1, xss[i]))
		}
	}
}
