package cg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatedSplitMRO is TOP: producer A under a disable gate, and B splitting A's
// output. shape picks A (a map call, the mapping SUB, the pass-through PASS),
// gate binds the disable to the literal input (lit) or a stage output (rt).
func gatedSplitMRO(shape, gate, skip, vals string) string {
	disabled := "self.skip"
	if gate == "rt" {
		disabled = "GATE.off"
	}

	producer := map[string]string{
		"chain": "map call SCALE as A(value = split self.vals,) using (disabled = " + disabled + ",)\n" +
			"map call SCALE as B(value = split A.scaled,)\n",
		"sub": "call SUB as A(xs = self.vals,) using (disabled = " + disabled + ",)\n" +
			"map call SCALE as B(value = split A.ys,)\n",
		"pass": "call PASS as A(xs = self.vals,) using (disabled = " + disabled + ",)\n" +
			"map call SCALE as B(value = split A.ys,)\n",
	}[shape]

	return `stage GATE(in bool v, out bool off, src py "stages/gate",)
stage SCALE(in int value, out int scaled, src py "stages/scale",)
pipeline SUB(in int[] xs, out int[] ys,)
{
    map call SCALE(value = split self.xs,)
    return (ys = SCALE.scaled,)
}
pipeline PASS(in int[] xs, out int[] ys,)
{
    return (ys = self.xs,)
}
pipeline TOP(in int[] vals, in bool skip, out int[] b,)
{
    call GATE(v = self.skip,)
` + producer + `    return (b = B.scaled,)
}
call TOP(vals = ` + vals + `, skip = ` + skip + `,)
`
}

// readSourceText reads src as an entry .mro, as a transpile reads it.
func readSourceText(t *testing.T, src string) error {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadSource(path, []string{dir})

	return err
}

// TestReadSourceRefusesAsMrp runs the #383 probe matrix: every shape under a
// literal and a runtime gate, each false and true, over an empty and a
// non-empty source. refused is what real mrp (martian 4a558e7dd93b) did with
// each variant: refuse it before any stage ran with "TOP.B input value: MRO
// unexpected array", or run it.
func TestReadSourceRefusesAsMrp(t *testing.T) {
	cases := []struct {
		shape, gate, skip, vals string
		refused                 bool
	}{
		{"chain", "lit", "false", "[]", false},
		{"chain", "lit", "false", "[1, 2]", false},
		{"chain", "lit", "true", "[]", false},
		{"chain", "lit", "true", "[1, 2]", false},
		{"chain", "rt", "false", "[]", false},
		{"chain", "rt", "false", "[1, 2]", true},
		{"chain", "rt", "true", "[]", false},
		{"chain", "rt", "true", "[1, 2]", true},
		{"sub", "lit", "false", "[]", false},
		{"sub", "lit", "false", "[1, 2]", false},
		{"sub", "lit", "true", "[]", false},
		{"sub", "lit", "true", "[1, 2]", false},
		{"sub", "rt", "false", "[]", false},
		{"sub", "rt", "false", "[1, 2]", true},
		{"sub", "rt", "true", "[]", false},
		{"sub", "rt", "true", "[1, 2]", true},
		{"pass", "lit", "false", "[]", false},
		{"pass", "lit", "false", "[1, 2]", false},
		{"pass", "lit", "true", "[]", false},
		{"pass", "lit", "true", "[1, 2]", false},
		{"pass", "rt", "false", "[]", true},
		{"pass", "rt", "false", "[1, 2]", true},
		{"pass", "rt", "true", "[]", true},
		{"pass", "rt", "true", "[1, 2]", true},
	}

	for _, tc := range cases {
		t.Run(tc.shape+"/"+tc.gate+"/"+tc.skip+"/"+tc.vals, func(t *testing.T) {
			err := readSourceText(t, gatedSplitMRO(tc.shape, tc.gate, tc.skip, tc.vals))

			if !tc.refused {
				if err != nil {
					t.Fatalf("mrp runs this variant; ReadSource refused it: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatal("mrp refuses this variant; ReadSource accepted it")
			}

			for _, want := range []string{"TOP.B input value:", "MRO unexpected array", "pipeline.mro:"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

const gateStages = `stage FLAG(
    in  bool b,
    out bool on,
    src py   "stages/flag",
)

stage WORK(
    in  int x,
    out int y,
    src py  "stages/work",
)

pipeline GATED(
    in  int  x,
    in  bool b,
    out int  y,
)
{
    call WORK(
        x = self.x,
    ) using (
        disabled = self.b,
    )

    return (
        y = WORK.y,
    )
}
`

// TestReadSourceRefusesStaticNullGate checks that a disable gate whose value
// is known before any stage runs to be null, or to be disabled with a call, is
// refused with mrp's call-graph binding error, while a gate on a stage output,
// which is null only at run time, resolves (#253).
func TestReadSourceRefusesStaticNullGate(t *testing.T) {
	const nullGate = "disabled cannot be bound to a null value"

	cases := []struct {
		name, pipeline, wantErr string
	}{
		{"invocation null", `pipeline P(in int x, in bool b, out int y,)
{
    call GATED(x = self.x, b = self.b,)
    return (y = GATED.y,)
}

call P(x = 3, b = null,)
`, nullGate},
		{"literal null into a sub-pipeline input", `pipeline P(in int x, out int y,)
{
    call GATED(x = self.x, b = null,)
    return (y = GATED.y,)
}

call P(x = 3,)
`, nullGate},
		{"output of a statically disabled call", `pipeline P(in int x, in bool b, in bool off, out int y,)
{
    call FLAG(b = self.b,) using (disabled = self.off,)
    call GATED(x = self.x, b = FLAG.on,)
    return (y = GATED.y,)
}

call P(x = 3, b = false, off = true,)
`, nullGate},
		{"output of a runtime-gated call", `pipeline P(in int x, in bool b, out int y,)
{
    call FLAG as OFF(b = self.b,)
    call FLAG(b = self.b,) using (disabled = OFF.on,)
    call GATED(x = self.x, b = FLAG.on,)
    return (y = GATED.y,)
}

call P(x = 3, b = false,)
`, "would be disabled at runtime"},
		{"runtime null from a stage output", `pipeline P(in int x, in bool b, out int y,)
{
    call FLAG(b = self.b,)
    call GATED(x = self.x, b = FLAG.on,)
    return (y = GATED.y,)
}

call P(x = 3, b = null,)
`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := readSourceText(t, gateStages+"\n"+tc.pipeline)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ReadSource: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ReadSource error = %v, want mrp's binding error containing %q", err, tc.wantErr)
			}
		})
	}
}
