package bind_test

import (
	"encoding/json"
	"testing"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// keyOrderTable declares structs whose field order is not alphabetical, so
// declared, producer and sorted key orders all differ.
func keyOrderTable() *types.Table {
	return types.NewTable(map[string]*ir.StructType{
		"In": {Name: "In", Fields: []ir.Param{
			{Name: "z", BaseType: "int"},
			{Name: "a", BaseType: "int"},
		}},
		"Box": {Name: "Box", Fields: []ir.Param{
			{Name: "q", BaseType: "In"},
			{Name: "b", BaseType: "int"},
		}},
	})
}

// prodOuts is PROD's _outs from an mrp 4a558e7 run whose stage wrote each
// value in the order shown: w is a Wide(z, a, m) and bx's q a Wide too.
const prodOuts = `{"a":2,"bx":{"b":3,"q":{"a":2,"m":9,"z":1}},"fl":{"a":2,"z":1},"m":9,` +
	`"s":{"z":1,"a":2},"um":{"y":1,"b":{"d":1,"c":2}},"w":{"a":2,"m":9,"z":1},"z":1}`

func prodRef(output string) *bind.Ref {
	return &bind.Ref{Kind: ir.RefKindCall, ID: "PROD", Output: output}
}

// decodeArgs decodes resolved args the way mre reads a bundle: each value with
// its objects in source order.
func decodeArgs(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()

	v, err := types.DecodeJSON(raw)
	if err != nil {
		t.Fatalf("decode args: %v", err)
	}

	o, ok := types.AsObject(v)
	if !ok {
		t.Fatalf("args %s are not an object", raw)
	}

	args := make(map[string]any, len(o))
	for _, m := range o {
		args[m.Key] = m.Value
	}

	return args
}

func encodeEach(t *testing.T, args map[string]any) map[string]string {
	t.Helper()

	out := make(map[string]string, len(args))

	for k, v := range args {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", k, err)
		}

		out[k] = string(raw)
	}

	return out
}

// TestNarrowKeyOrderMatchesMrp pins each bound value's key order to CONS's
// _args from the same mrp run: a projection keeps its producer's order unless
// FilterJson filters it, which rebuilds it in declared order; an object or
// array built from bindings, and a typed map built from them, has its keys
// sorted with each member filtered where it resolves; a whole-call ref keeps
// the declared fields with their keys sorted.
func TestNarrowKeyOrderMatchesMrp(t *testing.T) {
	spec := bind.Spec{
		"s1": {Ref: prodRef("s")},
		"s2": {Ref: prodRef("w")},
		"b1": {Object: map[string]bind.Entry{"q": {Ref: prodRef("s")}, "b": {Literal: json.RawMessage("3")}}},
		"b2": {Object: map[string]bind.Entry{"q": {Ref: prodRef("w")}, "b": {Literal: json.RawMessage("3")}}},
		"s3": {Ref: prodRef("")},
		"m1": {Ref: prodRef("um")},
		"b3": {Ref: prodRef("bx")},
		"s4": {Ref: prodRef("fl")},
		"arr": {Array: []bind.Entry{{Object: map[string]bind.Entry{
			"q": {Ref: prodRef("s")}, "b": {Literal: json.RawMessage("1")},
		}}}},
		"mp": {Object: map[string]bind.Entry{"k2": {Ref: prodRef("s")}, "k1": {Ref: prodRef("w")}}},
	}
	params := []ir.Param{
		{Name: "s1", BaseType: "In"},
		{Name: "s2", BaseType: "In"},
		{Name: "b1", BaseType: "Box"},
		{Name: "b2", BaseType: "Box"},
		{Name: "s3", BaseType: "In"},
		{Name: "m1", BaseType: "map"},
		{Name: "b3", BaseType: "Box"},
		{Name: "s4", BaseType: "In"},
		{Name: "arr", BaseType: "Box", ArrayDim: 1},
		{Name: "mp", BaseType: "In", MapDim: 1},
	}

	raw, err := bind.Resolve(spec, nil, map[string]json.RawMessage{"PROD": json.RawMessage(prodOuts)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := encodeEach(t, bind.Narrow(spec, decodeArgs(t, raw), params, keyOrderTable()))

	want := map[string]string{
		"arr": `[{"b":1,"q":{"z":1,"a":2}}]`,
		"b1":  `{"b":3,"q":{"z":1,"a":2}}`,
		"b2":  `{"b":3,"q":{"z":1,"a":2}}`,
		"b3":  `{"q":{"z":1,"a":2},"b":3}`,
		"m1":  `{"y":1,"b":{"d":1,"c":2}}`,
		"mp":  `{"k1":{"z":1,"a":2},"k2":{"z":1,"a":2}}`,
		"s1":  `{"z":1,"a":2}`,
		"s2":  `{"z":1,"a":2}`,
		"s3":  `{"a":2,"z":1}`,
		"s4":  `{"a":2,"z":1}`,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("bound key order differs from mrp's (-want +got):\n%s", diff)
	}
}

// TestNarrowWholeRefToMappedCall checks a whole ref to a mapped call filters
// each fork's outputs struct with its keys sorted, as mrp resolves the ref per
// fork (resolveMerge, then LazyArgumentMap.filter).
func TestNarrowWholeRefToMappedCall(t *testing.T) {
	tbl := keyOrderTable()
	cases := []struct {
		name, fork, src, want string
		p                     ir.Param
	}{
		{"array", bind.ForkArray, `[{"z":1,"a":2,"m":3}]`, `[{"a":2,"z":1}]`, ir.Param{BaseType: "In", ArrayDim: 1}},
		{"map", bind.ForkMap, `{"k2":{"z":1,"a":2},"k1":{"z":3,"a":4}}`, `{"k2":{"a":2,"z":1},"k1":{"a":4,"z":3}}`, ir.Param{BaseType: "In", MapDim: 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.p.Name = "v"
			spec := bind.Spec{"v": {Ref: &bind.Ref{Kind: ir.RefKindCall, ID: "M", Fork: tc.fork}}}
			args := decodeArgs(t, json.RawMessage(`{"v":`+tc.src+`}`))

			got := encodeEach(t, bind.Narrow(spec, args, []ir.Param{tc.p}, tbl))["v"]
			if got != tc.want {
				t.Errorf("Narrow = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestForkSpecElement checks a split binding becomes its fork's element
// binding: an array or map literal's entry, a whole ref to a mapped call's
// per-fork struct, or an element of a resolved value, while a broadcast
// binding is kept.
func TestForkSpecElement(t *testing.T) {
	elem0 := bind.Entry{Object: map[string]bind.Entry{"q": {Ref: prodRef("s")}}}
	ref := bind.Entry{Ref: prodRef("xs")}
	spec := bind.Spec{
		"arr":   {Split: true, Array: []bind.Entry{elem0, ref}},
		"map":   {Split: true, Object: map[string]bind.Entry{"k": ref}},
		"whole": {Split: true, Ref: &bind.Ref{Kind: ir.RefKindCall, ID: "M", Fork: bind.ForkArray}},
		"val":   {Split: true, Ref: prodRef("xs")},
		"bcast": ref,
	}

	got := bind.ForkSpec(spec, 0, "k")

	want := bind.Spec{
		"arr":   elem0,
		"map":   ref,
		"whole": {Ref: &bind.Ref{Kind: ir.RefKindCall, ID: "M"}},
		"val":   {},
		"bcast": ref,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ForkSpec mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(bind.Entry{}, bind.ForkSpec(spec, -1, "")["arr"]); diff != "" {
		t.Errorf("an unindexed element of an array literal (-want +got):\n%s", diff)
	}
}

// TestNarrowPassesUnboundArgs checks args with no receiving param pass through
// and the input is not mutated.
func TestNarrowPassesUnboundArgs(t *testing.T) {
	args := decodeArgs(t, json.RawMessage(`{"s":{"m":0,"a":1,"z":2},"other":{"y":1,"x":2}}`))

	got := encodeEach(t, bind.Narrow(bind.Spec{}, args, []ir.Param{{Name: "s", BaseType: "In"}}, keyOrderTable()))

	want := map[string]string{"s": `{"z":2,"a":1}`, "other": `{"y":1,"x":2}`}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Narrow mismatch (-want +got):\n%s", diff)
	}

	if s := encodeEach(t, args)["s"]; s != `{"m":0,"a":1,"z":2}` {
		t.Errorf("Narrow mutated its input: %s", s)
	}
}
