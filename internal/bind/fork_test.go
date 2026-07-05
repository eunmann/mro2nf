package bind_test

import (
	"encoding/json"
	"testing"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/google/go-cmp/cmp"
)

func TestResolveForks(t *testing.T) {
	spec := bind.Spec{
		"value":  {Ref: &bind.Ref{Kind: "self", ID: "values"}, Split: true},
		"factor": {Ref: &bind.Ref{Kind: "self", ID: "factor"}},
	}
	pipeArgs := json.RawMessage(`{"values":[1,2,3],"factor":10}`)

	forks, _, err := bind.ResolveForks(spec, pipeArgs, nil, false, bind.AllForks)
	if err != nil {
		t.Fatalf("resolve forks: %v", err)
	}
	if len(forks) != 3 {
		t.Fatalf("forks = %d, want 3", len(forks))
	}

	wantValues := []string{"1", "2", "3"}
	for i, wantValue := range wantValues {
		var got map[string]json.RawMessage
		if err := json.Unmarshal(forks[i], &got); err != nil {
			t.Fatalf("unmarshal fork %d: %v", i, err)
		}

		if string(got["value"]) != wantValue {
			t.Errorf("fork %d value = %s, want %s", i, got["value"], wantValue)
		}
		if string(got["factor"]) != "10" {
			t.Errorf("fork %d factor = %s, want 10 (broadcast)", i, got["factor"])
		}
	}
}

// TestResolveElementVerbatim checks the two halves of ResolveElement's
// contract in isolation: the pre-sliced element lands at the split param
// byte-for-byte (numeric lexemes like 1e5, -0.0, and >int64 integers must not
// be re-encoded — they are json.RawMessage all the way), and every broadcast
// binding resolves to exactly the bytes Resolve produces for it.
func TestResolveElementVerbatim(t *testing.T) {
	spec := bind.Spec{
		"v":   {Ref: &bind.Ref{Kind: "self", ID: "vals"}, Split: true},
		"f":   {Ref: &bind.Ref{Kind: "self", ID: "factor"}},
		"lbl": {Literal: json.RawMessage(`"report"`)},
	}
	pipeArgs := json.RawMessage(`{"vals":[1],"factor":10}`)
	// Compact on purpose: the args marshal strips insignificant whitespace but
	// must preserve every value lexeme.
	element := json.RawMessage(`{"deep":[1e5,-0.0,12345678901234567890,0.1000000000000000055511151231257827]}`)

	raw, err := bind.ResolveElement(spec, pipeArgs, nil, element)
	if err != nil {
		t.Fatalf("resolve element: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal element args: %v", err)
	}

	if string(got["v"]) != string(element) {
		t.Errorf("split param = %s, want the element verbatim %s", got["v"], element)
	}

	// Broadcast parity with Resolve: the non-split bindings of the same spec
	// resolve to identical bytes through either entry point.
	wantRaw, err := bind.Resolve(bind.Spec{"f": spec["f"], "lbl": spec["lbl"]}, pipeArgs, nil)
	if err != nil {
		t.Fatalf("resolve broadcasts: %v", err)
	}

	var want map[string]json.RawMessage
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("unmarshal broadcast args: %v", err)
	}

	for param, wantVal := range want {
		if string(got[param]) != string(wantVal) {
			t.Errorf("broadcast %q = %s, want Resolve's %s", param, got[param], wantVal)
		}
	}
}

// TestResolveElementEquivalence pins the equivalence property the O(1)
// native-scatter path (#99) rests on: for every fork i,
// ResolveElement(spec, ..., element_i) == ResolveForks(spec, ..., only=i)[i]
// == ResolveForks(spec, ..., AllForks)[i], byte for byte — where element_i is
// the raw slice of the split collection at fork i (array order; sorted map
// keys), exactly what the driver hands each instance. It also pins the
// only=i marshal arm: exactly slot i is marshaled, every other slot stays
// nil, and the returned keys match the full resolve's.
func TestResolveElementEquivalence(t *testing.T) {
	callOuts := map[string]json.RawMessage{
		"MAKE": json.RawMessage(`{"m":{"a":{"x":1},"b":{"x":2}},"arr":[{"mean":1e5},{"mean":-0.0}],"sum":14}`),
	}

	cases := []struct {
		name       string
		spec       bind.Spec
		pipeArgs   string
		isMap      bool
		collection string
	}{
		{
			name: "array literal split with numeric edge lexemes",
			spec: bind.Spec{
				"n": {Literal: json.RawMessage(`[1e5,-0.0,9223372036854775807,12345678901234567890,0.1000000000000000055511151231257827,2.5e-3]`), Split: true},
				"f": {Ref: &bind.Ref{Kind: "self", ID: "factor"}},
			},
			pipeArgs:   `{"factor":10}`,
			collection: `[1e5,-0.0,9223372036854775807,12345678901234567890,0.1000000000000000055511151231257827,2.5e-3]`,
		},
		{
			name: "array ref split of structs with map-projection broadcast",
			spec: bind.Spec{
				"s":  {Ref: &bind.Ref{Kind: "call", ID: "MAKE", Output: "arr"}, Split: true},
				"xs": {Ref: &bind.Ref{Kind: "call", ID: "MAKE", Output: "m.x", MapDepth: 1}},
			},
			pipeArgs:   `{}`,
			collection: `[{"mean":1e5},{"mean":-0.0}]`,
		},
		{
			name: "map ref split with unicode key order",
			spec: bind.Spec{
				"v": {Ref: &bind.Ref{Kind: "self", ID: "m"}, Split: true},
				"f": {Literal: json.RawMessage(`2`)},
			},
			pipeArgs:   `{"m":{"b":{"y":2},"a":{"y":1},"é":{"y":3}}}`,
			isMap:      true,
			collection: `{"b":{"y":2},"a":{"y":1},"é":{"y":3}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pa := json.RawMessage(tc.pipeArgs)

			full, keys, err := bind.ResolveForks(tc.spec, pa, callOuts, tc.isMap, bind.AllForks)
			if err != nil {
				t.Fatalf("resolve all forks: %v", err)
			}

			elements := sliceElements(t, tc.collection, keys)
			if len(elements) != len(full) {
				t.Fatalf("sliced %d elements for %d forks", len(elements), len(full))
			}

			for i := range full {
				gotElem, err := bind.ResolveElement(tc.spec, pa, callOuts, elements[i])
				if err != nil {
					t.Fatalf("resolve element %d: %v", i, err)
				}

				if string(gotElem) != string(full[i]) {
					t.Errorf("fork %d: ResolveElement = %s, want ResolveForks[%d] = %s", i, gotElem, i, full[i])
				}

				assertOnlyArm(t, tc.spec, pa, callOuts, tc.isMap, i, full, keys)
			}
		})
	}
}

// assertOnlyArm checks ResolveForks' single-fork marshal arm at fork i: it
// marshals exactly slot i to the full resolve's bytes, leaves every other slot
// nil, and returns the full resolve's keys.
func assertOnlyArm(t *testing.T, spec bind.Spec, pa json.RawMessage, callOuts map[string]json.RawMessage, isMap bool, i int, full []json.RawMessage, keys []string) {
	t.Helper()

	only, onlyKeys, err := bind.ResolveForks(spec, pa, callOuts, isMap, i)
	if err != nil {
		t.Fatalf("resolve only=%d: %v", i, err)
	}

	if diff := cmp.Diff(keys, onlyKeys); diff != "" {
		t.Errorf("only=%d keys mismatch (-all +only):\n%s", i, diff)
	}

	if len(only) != len(full) {
		t.Fatalf("only=%d forks = %d, want %d", i, len(only), len(full))
	}

	if string(only[i]) != string(full[i]) {
		t.Errorf("fork %d: only-marshal = %s, want %s", i, only[i], full[i])
	}

	for j, slot := range only {
		if j != i && slot != nil {
			t.Errorf("only=%d marshaled slot %d = %s, want nil", i, j, slot)
		}
	}
}

// sliceElements slices a split collection the way the scatter driver does:
// array elements in order, or map values in the fork-key order keys gives
// (keys nil selects array mode).
func sliceElements(t *testing.T, collection string, keys []string) []json.RawMessage {
	t.Helper()

	if keys == nil {
		var arr []json.RawMessage
		if err := json.Unmarshal(json.RawMessage(collection), &arr); err != nil {
			t.Fatalf("slice array collection: %v", err)
		}

		return arr
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(json.RawMessage(collection), &m); err != nil {
		t.Fatalf("slice map collection: %v", err)
	}

	out := make([]json.RawMessage, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}

	return out
}

func TestResolveForksNoSplit(t *testing.T) {
	spec := bind.Spec{"factor": {Ref: &bind.Ref{Kind: "self", ID: "factor"}}}

	_, _, err := bind.ResolveForks(spec, json.RawMessage(`{"factor":10}`), nil, false, bind.AllForks)
	if err == nil {
		t.Fatal("expected error for map call with no split binding")
	}
}

func TestResolveForksMap(t *testing.T) {
	// Forking over a map<T> yields one fork per key, in sorted key order, and
	// returns the keys so the result can be rebuilt as a map.
	spec := bind.Spec{
		"v": {Ref: &bind.Ref{Kind: "self", ID: "m"}, Split: true},
	}
	pipeArgs := json.RawMessage(`{"m":{"b":2,"a":1}}`)

	forks, keys, err := bind.ResolveForks(spec, pipeArgs, nil, true, bind.AllForks)
	if err != nil {
		t.Fatalf("resolve forks: %v", err)
	}

	if diff := cmp.Diff([]string{"a", "b"}, keys); diff != "" {
		t.Errorf("keys mismatch (-want +got):\n%s", diff)
	}
	if len(forks) != 2 {
		t.Fatalf("forks = %d, want 2", len(forks))
	}

	var fork0 map[string]json.RawMessage
	_ = json.Unmarshal(forks[0], &fork0)
	if string(fork0["v"]) != "1" {
		t.Errorf("fork[0] (key a) v = %s, want 1", fork0["v"])
	}
}

func TestMergeMap(t *testing.T) {
	outs := []json.RawMessage{
		json.RawMessage(`{"w":2}`),
		json.RawMessage(`{"w":4}`),
	}

	merged, err := bind.Merge([]string{"w"}, outs, []string{"a", "b"}, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if got := string(merged); got != `{"w":{"a":2,"b":4}}` {
		t.Errorf("map merge = %s, want {\"w\":{\"a\":2,\"b\":4}}", got)
	}
}

func TestMergeEmpty(t *testing.T) {
	// A zero-fork ARRAY map call yields an empty array per output, matching
	// Martian's runtime merge (marshallerArray{} -> []) for an empty or null
	// typed-array source; keys nil signals array mode.
	merged, err := bind.Merge([]string{"scaled"}, nil, nil, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if got := string(merged); got != `{"scaled":[]}` {
		t.Errorf("merge of zero array forks = %s, want {\"scaled\":[]}", got)
	}

	// A zero-fork MAP map call (non-nil empty keys) yields an empty object.
	merged, err = bind.Merge([]string{"scaled"}, nil, []string{}, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if got := string(merged); got != `{"scaled":{}}` {
		t.Errorf("merge of zero map forks = %s, want {\"scaled\":{}}", got)
	}
}

// TestMergeEmptyNull pins the invocation-known-empty rule (#99): with
// emptyNull, ZERO forks merge every output to null (mrp's static resolver
// prunes a statically-empty fork to null), array and map mode alike — while a
// non-empty merge is unaffected by the flag.
func TestMergeEmptyNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
	}{
		{"array mode", nil},
		{"map mode", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := bind.Merge([]string{"scaled"}, nil, tc.keys, true)
			if err != nil {
				t.Fatalf("merge: %v", err)
			}

			if got := string(merged); got != `{"scaled":null}` {
				t.Errorf("emptyNull zero-fork merge = %s, want {\"scaled\":null}", got)
			}
		})
	}

	// Non-zero forks: the flag must not perturb the merged collection.
	outs := []json.RawMessage{json.RawMessage(`{"scaled":10}`), json.RawMessage(`{"scaled":20}`)}

	merged, err := bind.Merge([]string{"scaled"}, outs, nil, true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if got := string(merged); got != `{"scaled":[10,20]}` {
		t.Errorf("emptyNull non-empty merge = %s, want {\"scaled\":[10,20]}", got)
	}
}

func TestMerge(t *testing.T) {
	outs := []json.RawMessage{
		json.RawMessage(`{"scaled":10}`),
		json.RawMessage(`{"scaled":20}`),
		json.RawMessage(`{"scaled":30}`),
	}

	merged, err := bind.Merge([]string{"scaled"}, outs, nil, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	var got struct {
		Scaled []int `json:"scaled"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Scaled) != 3 || got.Scaled[0] != 10 || got.Scaled[2] != 30 {
		t.Errorf("merged scaled = %v, want [10 20 30]", got.Scaled)
	}
}
