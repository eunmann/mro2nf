package bind_test

import (
	"encoding/json"
	"testing"

	"github.com/eunmann/martian-nextflow/internal/bind"
)

func TestResolveForks(t *testing.T) {
	spec := bind.Spec{
		"value":  {Ref: &bind.Ref{Kind: "self", ID: "values"}, Split: true},
		"factor": {Ref: &bind.Ref{Kind: "self", ID: "factor"}},
	}
	pipeArgs := json.RawMessage(`{"values":[1,2,3],"factor":10}`)

	forks, err := bind.ResolveForks(spec, pipeArgs, nil)
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

	_, err := bind.ResolveForks(spec, json.RawMessage(`{"factor":10}`), nil)
	if err == nil {
		t.Fatal("expected error for map call with no split binding")
	}
}

func TestMerge(t *testing.T) {
	outs := []json.RawMessage{
		json.RawMessage(`{"scaled":10}`),
		json.RawMessage(`{"scaled":20}`),
		json.RawMessage(`{"scaled":30}`),
	}

	merged, err := bind.Merge([]string{"scaled"}, outs)
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
