package cg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// postFields posts the instance outs stand for with Fields and Gates set, and
// returns the fields dir and the gates the post wrote.
func (f *fixture) postFields(outs string, man types.Manifest) (string, string) {
	f.t.Helper()

	fields, gates := f.next("fields"), f.next("gates")
	if err := f.g.Post(PostSpec{Args: outs, Outs: outs, Manifest: man, Forks: f.next("forks"), Fields: fields, Gates: gates}); err != nil {
		f.t.Fatal(err)
	}

	raw, err := os.ReadFile(gates)
	if err != nil {
		f.t.Fatal(err)
	}

	return fields, string(raw)
}

// TestPostFields: SRC's post writes one stamped bundle per output, and USE,
// staging only the y bundle, gets the _args mrp gave chain_gate's USE
// (CH/USE/fork0/split/_args: {"k":3,"y":4}).
func TestPostFields(t *testing.T) {
	f := loadFixture(t, "chain_gate")

	src := f.outs("CH.SRC", nil, `{"y":4,"flag":false}`)
	fields, _ := f.postFields(src, fixtureManifest(t, "chain_gate"))

	for field, want := range map[string]string{"y": `{"y":4}`, "flag": `{"flag":false}`} {
		dir := filepath.Join(fields, field)

		raw, err := shim.ReadBundle(dir)
		if err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff(want, string(raw)); d != "" {
			t.Errorf("SRC's %s bundle (-want +got):\n%s", field, d)
		}

		if in, err := f.g.ReadInstance(dir); err != nil || in.Node != "CH.SRC" {
			t.Errorf("SRC's %s bundle is stamped %+v (%v), want CH.SRC", field, in, err)
		}
	}

	_, got := f.args("CH.USE", filepath.Join(fields, "y"))
	wantArgs(t, "CH.USE", got, `{"k":3,"y":4}`)
}

// TestPostGates: a producer's post names the instances its outs disable:
// GATE when SRC.flag is true (mrp writes CH/GATE/fork0/_disabled), none
// when it is false, and none for a null gate, which mrp reads as false
// (null_gate: every gated call runs). A gate split per fork of a map call
// the producer feeds names each fork it disables by its ordinal
// (split_gate: mrp disables INNER/WORK/fork1, ITEM/WORK/fork0 and fork2),
// and so it does of a map call the entry forks, in the plan's order of the
// entry's elements (split_gate_entry: mrp disables INNER/WORK/fork0 and
// fork1, KEYED/WORK/fork_b and fork_c).
func TestPostGates(t *testing.T) {
	cases := []struct {
		fixture, node, outs, want string
	}{
		{"chain_gate", "CH.SRC", `{"y":4,"flag":true}`, "CH.GATE@@"},
		{"chain_gate", "CH.SRC", `{"y":4,"flag":false}`, ""},
		{"null_gate", "NULL_GATE.FLAG", `{"on":true,"cfg":{"flag":false,"k":1}}`, "NULL_GATE.DIRECT@@ NULL_GATE.MAPPED.WORK@@ NULL_GATE.SUB.WORK@@"},
		{"null_gate", "NULL_GATE.FLAG", `{"on":null,"cfg":{"flag":true,"k":1}}`, "NULL_GATE.FIELD@@"},
		{
			"split_gate", "SPLIT_GATE.SRC",
			`{"xs":[1,2,3],"skips":[false,true,false],"entries":[{"x":1,"skip":true},{"x":2,"skip":false},{"x":3,"skip":true}]}`,
			"SPLIT_GATE.INNER.WORK@SPLIT_GATE.INNER@1 SPLIT_GATE.ITEM.WORK@SPLIT_GATE.ITEM@0 SPLIT_GATE.ITEM.WORK@SPLIT_GATE.ITEM@2",
		},
		{
			"split_gate_entry", "SPLIT_GATE_ENTRY.SRC",
			`{"skips":[true,true,false],"flags":{"a":false,"b":true,"c":true}}`,
			"SPLIT_GATE_ENTRY.INNER.WORK@SPLIT_GATE_ENTRY.INNER@0 SPLIT_GATE_ENTRY.INNER.WORK@SPLIT_GATE_ENTRY.INNER@1 " +
				"SPLIT_GATE_ENTRY.KEYED.WORK@SPLIT_GATE_ENTRY.KEYED@1 SPLIT_GATE_ENTRY.KEYED.WORK@SPLIT_GATE_ENTRY.KEYED@2",
		},
	}

	for _, tc := range cases {
		f := loadFixture(t, tc.fixture)

		if _, got := f.postFields(f.outs(tc.node, nil, tc.outs), fixtureManifest(t, tc.fixture)); got != tc.want {
			t.Errorf("%s outs %s: gates %q, want %q", tc.node, tc.outs, got, tc.want)
		}
	}
}
