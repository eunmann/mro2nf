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
