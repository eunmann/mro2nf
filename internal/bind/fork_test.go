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

	forks, _, err := bind.ResolveForks(spec, pipeArgs, nil)
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

func TestResolveForksNoSplit(t *testing.T) {
	spec := bind.Spec{"factor": {Ref: &bind.Ref{Kind: "self", ID: "factor"}}}

	_, _, err := bind.ResolveForks(spec, json.RawMessage(`{"factor":10}`), nil)
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

	forks, keys, err := bind.ResolveForks(spec, pipeArgs, nil)
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

	merged, err := bind.Merge([]string{"w"}, outs, []string{"a", "b"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if got := string(merged); got != `{"w":{"a":2,"b":4}}` {
		t.Errorf("map merge = %s, want {\"w\":{\"a\":2,\"b\":4}}", got)
	}
}

func TestMergeEmpty(t *testing.T) {
	// An empty map call yields null per output (Martian's null-map-call
	// semantics), not an empty array.
	merged, err := bind.Merge([]string{"scaled"}, nil, nil)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if got := string(merged); got != `{"scaled":null}` {
		t.Errorf("merge of zero forks = %s, want {\"scaled\":null}", got)
	}
}

func TestMerge(t *testing.T) {
	outs := []json.RawMessage{
		json.RawMessage(`{"scaled":10}`),
		json.RawMessage(`{"scaled":20}`),
		json.RawMessage(`{"scaled":30}`),
	}

	merged, err := bind.Merge([]string{"scaled"}, outs, nil)
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
