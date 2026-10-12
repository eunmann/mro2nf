package cg

import (
	"errors"
	"slices"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// gateDecls are a producer and a stage gated by a sub-pipeline input, under
// a map call that splits the producer's outs.
const gateDecls = `
struct Item(
    int  x,
    bool skip,
)

stage SRC(
    out int[]  xs,
    out bool[] skips,
    out Item[] items,
    src py     "stages/src",
)

stage WORK(
    in  int x,
    out int y,
    src py  "stages/work",
)

pipeline INNER(
    in  int  x,
    in  bool skip,
    out int  y,
)
{
    call WORK(
        x = self.x,
    ) using (
        disabled = self.skip,
    )

    return (
        y = WORK.y,
    )
}

pipeline ITEM(
    in  Item item,
    out int  y,
)
{
    call WORK(
        x = self.item.x,
    ) using (
        disabled = self.item.skip,
    )

    return (
        y = WORK.y,
    )
}
`

// TestTopologyForkedGate: a disable binding that splits its producer's outs
// over a map call the producer feeds resolves to one value per fork of the
// gated node, so the gate carries the map call's root, over whose forks the
// producer's post posts the binding's value.
func TestTopologyForkedGate(t *testing.T) {
	for name, call := range map[string]string{
		"zip": `
pipeline P(
    out int[] ys,
)
{
    call SRC()

    map call INNER(
        x    = split SRC.xs,
        skip = split SRC.skips,
    )

    return (
        ys = INNER.y,
    )
}

call P()
`,
		"struct": `
pipeline P(
    out int[] ys,
)
{
    call SRC()

    map call ITEM(
        item = split SRC.items,
    )

    return (
        ys = ITEM.y,
    )
}

call P()
`,
	} {
		g, err := Compile([]byte(gateDecls+call), "pipeline.mro", []string{t.TempDir()})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		topo, err := g.Topology()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		root := map[string]string{"zip": "P.INNER", "struct": "P.ITEM"}[name]

		i := slices.IndexFunc(topo.Nodes, func(n TopoNode) bool { return n.Fqid == root+".WORK" })
		if i < 0 {
			t.Fatalf("%s: no %s.WORK node", name, root)
		}

		if d := cmp.Diff([]Gate{{Node: "P.SRC", Roots: []string{root}}}, topo.Nodes[i].Gates); d != "" {
			t.Errorf("%s: WORK's gates (-want +got):\n%s", name, d)
		}

		if len(topo.Nodes[i].Reads) != 0 {
			t.Errorf("%s: WORK reads %v, want only its fork elements, which carry the gate's element", name, topo.Nodes[i].Reads)
		}
	}
}

// TestTopologyRefusesForeignForkedGate: a disable binding forked on a map
// call whose forks another stage fixes resolves at forks its producer's
// post cannot enumerate, so the shape is refused rather than run with every
// fork enabled.
func TestTopologyRefusesForeignForkedGate(t *testing.T) {
	src := gateDecls + `
stage OTHER(
    out int[] xs,
    src py    "stages/other",
)

pipeline P(
    out int[] ys,
)
{
    call SRC()

    call OTHER()

    map call INNER(
        x    = split OTHER.xs,
        skip = split SRC.skips,
    )

    return (
        ys = INNER.y,
    )
}

call P()
`

	g, err := Compile([]byte(src), "pipeline.mro", []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.Topology(); !errors.Is(err, errShapeUnsupported) {
		t.Errorf("Topology = %v, want errShapeUnsupported", err)
	}
}

// TestTopologyZipOneProducer: a map call zipping two outputs of one stage
// is forked over the first, and each fork's element carries that fork's
// element of both, so a fork stages none of the stage's outs: O(1) bytes
// per fork, not the whole second output. A fork past the end of the
// shorter output reads null, as mrp reads it.
func TestTopologyZipOneProducer(t *testing.T) {
	src := gateDecls + `
stage PAIR(
    in  int  x,
    in  bool skip,
    out int  y,
    src py   "stages/pair",
)

pipeline P(
    out int[] ys,
)
{
    call SRC()

    map call PAIR(
        x    = split SRC.xs,
        skip = split SRC.skips,
    )

    return (
        ys = PAIR.y,
    )
}

call P()
`

	g, err := Compile([]byte(src), "pipeline.mro", []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	topo, err := g.Topology()
	if err != nil {
		t.Fatal(err)
	}

	i := slices.IndexFunc(topo.Nodes, func(n TopoNode) bool { return n.Fqid == "P.PAIR" })
	if i < 0 {
		t.Fatalf("no P.PAIR node in %+v", topo.Nodes)
	}

	if len(topo.Nodes[i].Reads) != 0 {
		t.Errorf("P.PAIR reads %v, want only its fork elements", topo.Nodes[i].Reads)
	}

	f := &fixture{t: t, g: g, dir: t.TempDir()}
	outs := f.outs("P.SRC", nil, `{"xs":[1,2,3],"skips":[false,true],"items":null}`)

	elems := elementDirs(t, f.post(outs, outs), "P.PAIR")
	if len(elems) != 3 {
		t.Fatalf("SRC posted %d forks of PAIR, want 3", len(elems))
	}

	for i, want := range []string{`{"out:skips":false,"value":1}`, `{"out:skips":true,"value":2}`, `{"value":3}`} {
		raw, err := shim.ReadBundle(elems[i])
		if err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff(want, string(raw)); d != "" {
			t.Errorf("fork %d element (-want +got):\n%s", i, d)
		}
	}

	_, got := f.args("P.PAIR", elems[1])
	wantArgs(t, "P.PAIR fork1", got, `{"skip":true,"x":2}`)

	_, got = f.args("P.PAIR", elems[2])
	wantArgs(t, "P.PAIR fork2", got, `{"skip":null,"x":3}`)
}

// TestTopologyZipPartners: a map call zipping outputs of different stages
// its master's fork element and the other stage's partner element
// at the same index, never the other stage's whole output, so a fork
// stages O(1) bytes of it, not O(N) (zip_runtime: LONG forks over TWO.xs
// and zips THREE.xs).
func TestTopologyZipPartners(t *testing.T) {
	f := loadFixture(t, "zip_runtime")

	topo, err := f.g.Topology()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]Zip{
		"ZIP_RUNTIME.LONG":        {{Root: "ZIP_RUNTIME.LONG", Node: "ZIP_RUNTIME.THREE", Output: "xs"}},
		"ZIP_RUNTIME.SHORT":       {{Root: "ZIP_RUNTIME.SHORT", Node: "ZIP_RUNTIME.TWO", Output: "xs"}},
		"ZIP_RUNTIME.ORDER":       {{Root: "ZIP_RUNTIME.ORDER", Node: "ZIP_RUNTIME.THREE", Output: "xs"}},
		"ZIP_RUNTIME.NULL_MASTER": {{Root: "ZIP_RUNTIME.NULL_MASTER", Node: "ZIP_RUNTIME.TWO", Output: "xs"}},
		"ZIP_RUNTIME.NULL_OTHER":  {{Root: "ZIP_RUNTIME.NULL_OTHER", Node: "ZIP_RUNTIME.NONE", Output: "xs"}},
	}

	for _, n := range topo.Nodes {
		if len(n.Reads) != 0 {
			t.Errorf("%s reads %v, want only its fork and partner elements", n.Fqid, n.Reads)
		}

		if d := cmp.Diff(want[n.Fqid], n.Zips); d != "" {
			t.Errorf("%s zips (-want +got):\n%s", n.Fqid, d)
		}
	}
}

// TestTopologyPreflights: each preflight of a pipeline gates every other
// call in it, and through a sub-pipeline every call within, the
// sub-pipeline's preflight too, but not the pipeline's other preflights; a
// mapped or disabled preflight gates as a plain one (mrp's run of
// preflight_order: TOP_CHECK, EMIT, SUB.CHECK, SUB.WORK).
func TestTopologyPreflights(t *testing.T) {
	for _, c := range []struct {
		fixture string
		want    map[string][]string
	}{
		{"preflight_order", map[string][]string{
			"TOP.TOP_CHECK": nil,
			"TOP.EMIT":      {"TOP.TOP_CHECK"},
			"TOP.SUB.CHECK": {"TOP.TOP_CHECK"},
			"TOP.SUB.WORK":  {"TOP.SUB.CHECK", "TOP.TOP_CHECK"},
		}},
		{"preflight_ungated", map[string][]string{
			"PU.CHECK_EACH":  nil,
			"PU.CHECK_FIRST": nil,
			"PU.DOUBLE":      {"PU.CHECK_EACH", "PU.CHECK_FIRST"},
		}},
	} {
		topo, err := loadFixture(t, c.fixture).g.Topology()
		if err != nil {
			t.Fatal(err)
		}

		got := map[string][]string{}
		for _, n := range topo.Nodes {
			got[n.Fqid] = n.Preflights
		}

		if d := cmp.Diff(c.want, got); d != "" {
			t.Errorf("%s preflights (-want +got):\n%s", c.fixture, d)
		}
	}
}
