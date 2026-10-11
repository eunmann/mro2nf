package cg

import (
	"errors"
	"slices"
	"testing"

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

// TestTopologyRefusesForkedGate: a disable binding that splits its
// producer's outs over a map call the producer is not forked on resolves to
// one value per fork of the gated node, which the producer's one gate value
// cannot carry, so the shape is refused rather than run with every fork
// enabled.
func TestTopologyRefusesForkedGate(t *testing.T) {
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

		if _, err := g.Topology(); !errors.Is(err, errShapeUnsupported) {
			t.Errorf("%s: Topology = %v, want errShapeUnsupported", name, err)
		}
	}
}

// TestTopologyZipOneProducer: a map call zipping two outputs of one stage
// is forked over the first, which the fork element serves, and reads the
// other from the stage's outs, as resolveSplit does.
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

	if d := cmp.Diff([]Read{{Node: "P.SRC", Fields: []string{"skips"}}}, topo.Nodes[i].Reads); d != "" {
		t.Errorf("P.PAIR's reads (-want +got):\n%s", d)
	}

	f := &fixture{t: t, g: g, dir: t.TempDir()}
	outs := f.outs("P.SRC", nil, `{"xs":[1,2],"skips":[false,true],"items":null}`)

	elems := elementDirs(t, f.post(outs, outs), "P.PAIR")
	if len(elems) != 2 {
		t.Fatalf("SRC posted %d forks of PAIR, want 2", len(elems))
	}

	_, got := f.args("P.PAIR", elems[1], outs)
	wantArgs(t, "P.PAIR fork1", got, `{"skip":true,"x":2}`)
}
