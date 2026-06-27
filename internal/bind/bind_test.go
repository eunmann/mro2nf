package bind_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eunmann/mro2nf/internal/bind"
)

// TestResolveMapProjection projects a struct field through a typed map<S>:
// CALL.m.x over {"m":{"a":{"x":1},"b":{"x":2}}} yields {"a":1,"b":2}.
func TestResolveMapProjection(t *testing.T) {
	spec := bind.Spec{
		"xs": {Ref: &bind.Ref{Kind: "call", ID: "MAKE", Output: "m.x", MapDepth: 1}},
	}
	callOuts := map[string]json.RawMessage{
		"MAKE": json.RawMessage(`{"m":{"a":{"x":1},"b":{"x":2}}}`),
	}

	raw, err := bind.Resolve(spec, nil, callOuts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got struct {
		Xs map[string]int `json:"xs"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if diff := cmp.Diff(map[string]int{"a": 1, "b": 2}, got.Xs); diff != "" {
		t.Errorf("map projection mismatch (-want +got):\n%s", diff)
	}
}

func TestResolve(t *testing.T) {
	spec := bind.Spec{
		"values": {Ref: &bind.Ref{Kind: "self", ID: "values"}},
		"sum":    {Ref: &bind.Ref{Kind: "call", ID: "SUM_SQUARES", Output: "sum"}},
		"label":  {Literal: json.RawMessage(`"report"`)},
	}
	pipeArgs := json.RawMessage(`{"values":[1,2,3],"disable_sq":false}`)
	callOuts := map[string]json.RawMessage{
		"SUM_SQUARES": json.RawMessage(`{"sum":14}`),
	}

	raw, err := bind.Resolve(spec, pipeArgs, callOuts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key, want := range map[string]string{
		"values": "[1,2,3]",
		"sum":    "14",
		"label":  `"report"`,
	} {
		if string(got[key]) != want {
			t.Errorf("%s = %s, want %s", key, got[key], want)
		}
	}
}

func TestResolveArrayProjection(t *testing.T) {
	// Map-then-gather: an upstream map call produces an array of structs, and a
	// downstream call projects a field through it (CALL.s.mean), which Martian
	// resolves to an array. Regression test for extract() crashing on arrays.
	spec := bind.Spec{
		"means": {Ref: &bind.Ref{Kind: "call", ID: "M", Output: "s.mean"}},
	}
	callOuts := map[string]json.RawMessage{
		"M": json.RawMessage(`{"s":[{"mean":1},{"mean":2},{"mean":3}]}`),
	}

	raw, err := bind.Resolve(spec, nil, callOuts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got struct {
		Means []int `json:"means"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []int{1, 2, 3}
	if diff := cmp.Diff(want, got.Means); diff != "" {
		t.Errorf("means mismatch (-want +got):\n%s", diff)
	}
}

func TestResolveNestedAndMissing(t *testing.T) {
	spec := bind.Spec{
		"deep":    {Ref: &bind.Ref{Kind: "self", ID: "cfg", Output: "a.b"}},
		"whole":   {Ref: &bind.Ref{Kind: "call", ID: "S", Output: ""}},
		"absent":  {Ref: &bind.Ref{Kind: "call", ID: "MISSING", Output: "x"}},
		"missing": {Ref: &bind.Ref{Kind: "self", ID: "cfg", Output: "nope"}},
	}
	pipeArgs := json.RawMessage(`{"cfg":{"a":{"b":42}}}`)
	callOuts := map[string]json.RawMessage{"S": json.RawMessage(`{"k":1}`)}

	raw, err := bind.Resolve(spec, pipeArgs, callOuts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	checks := map[string]string{
		"deep":    "42",
		"whole":   `{"k":1}`,
		"absent":  "null",
		"missing": "null",
	}
	for key, want := range checks {
		if string(got[key]) != want {
			t.Errorf("%s = %s, want %s", key, got[key], want)
		}
	}
}
