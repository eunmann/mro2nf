package bind_test

import (
	"encoding/json"
	"testing"

	"github.com/eunmann/martian-nextflow/internal/bind"
)

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
