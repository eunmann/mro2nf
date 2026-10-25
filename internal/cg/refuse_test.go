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

// TestGatedSplitTranspilesAsMrp runs the #383 probe matrix: every shape
// under a literal and a runtime gate, each false and true, over an empty
// and a non-empty source. Real mrp at Martian 50ad6c0 instantiates every
// variant, where 4a558e7 refused the runtime-gated splits of a non-empty
// source (#387), so each reads, and its call graph is within its witness's,
// which the transpile emits.
func TestGatedSplitTranspilesAsMrp(t *testing.T) {
	for _, shape := range []string{"chain", "sub", "pass"} {
		for _, gate := range []string{"lit", "rt"} {
			for _, skip := range []string{"false", "true"} {
				for _, vals := range []string{"[]", "[1, 2]"} {
					t.Run(shape+"/"+gate+"/"+skip+"/"+vals, func(t *testing.T) {
						dir := t.TempDir()
						path := filepath.Join(dir, "pipeline.mro")

						if err := os.WriteFile(path, []byte(gatedSplitMRO(shape, gate, skip, vals)), 0o600); err != nil {
							t.Fatal(err)
						}

						src, err := ReadSource(path, []string{dir})
						if err != nil {
							t.Fatalf("mrp instantiates this variant; ReadSource refused it: %v", err)
						}

						if err := checkCovered(src, dir); err != nil {
							t.Error(err)
						}
					})
				}
			}
		}
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
