package cg

import (
	"errors"
	"testing"
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
