package types_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

func decodeOrdered(t *testing.T, s string) any {
	t.Helper()

	v, err := types.DecodeJSON([]byte(s))
	if err != nil {
		t.Fatalf("DecodeJSON(%s): %v", s, err)
	}

	return v
}

func encode(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}

	return string(raw)
}

// TestDecodeJSONKeepsOrder checks objects decode with their source key order
// at every depth and numbers keep their source token, so a decoded value
// re-encodes to its source bytes.
func TestDecodeJSONKeepsOrder(t *testing.T) {
	src := `{"b":1,"a":{"z":2.0,"y":[{"q":1E6,"p":null}],"x":{}},"c":[true,"s"]}`

	got := decodeOrdered(t, src)

	want := types.Object{
		{Key: "b", Value: json.Number("1")},
		{Key: "a", Value: types.Object{
			{Key: "z", Value: json.Number("2.0")},
			{Key: "y", Value: []any{types.Object{{Key: "q", Value: json.Number("1E6")}, {Key: "p", Value: nil}}}},
			{Key: "x", Value: types.Object{}},
		}},
		{Key: "c", Value: []any{true, "s"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("decoded value mismatch (-want +got):\n%s", diff)
	}

	if enc := encode(t, got); enc != src {
		t.Errorf("re-encoded %s, want the source %s", enc, src)
	}
}

// TestDecodeJSONDuplicateKey checks a repeated key keeps its first position and
// its last value, as Python's json and encoding/json resolve it.
func TestDecodeJSONDuplicateKey(t *testing.T) {
	if got := encode(t, decodeOrdered(t, `{"a":1,"b":2,"a":3}`)); got != `{"a":3,"b":2}` {
		t.Errorf("got %s, want {\"a\":3,\"b\":2}", got)
	}
}

// TestDecodeJSONMalformed checks malformed JSON is an error, not a partial value.
func TestDecodeJSONMalformed(t *testing.T) {
	for _, src := range []string{`{"a" 1}`, `{"a":1`, `[1,`, `{1:2}`, ``} {
		if _, err := types.DecodeJSON([]byte(src)); err == nil {
			t.Errorf("DecodeJSON(%q) succeeded, want an error", src)
		}
	}
}

// TestAsObject checks both object forms read as an Object: an Object as it is,
// a map with its keys sorted.
func TestAsObject(t *testing.T) {
	o := types.Object{{Key: "b", Value: 1}, {Key: "a", Value: 2}}

	if got, ok := types.AsObject(o); !ok || encode(t, got) != `{"b":1,"a":2}` {
		t.Errorf("AsObject(Object) = %v, %v; want it unchanged", got, ok)
	}

	if got, ok := types.AsObject(map[string]any{"b": 1, "a": 2}); !ok || encode(t, got) != `{"a":2,"b":1}` {
		t.Errorf("AsObject(map) = %v, %v; want its keys sorted", got, ok)
	}

	if _, ok := types.AsObject([]any{}); ok {
		t.Error("AsObject accepted an array")
	}
}

// orderTable declares structs whose field order is not alphabetical, so a
// result in declared order, source order and sorted order all differ.
func orderTable(t *testing.T) *types.Table {
	t.Helper()

	return types.NewTable(&compile(t, `
struct In(
    int z,
    int a,
)

struct Box(
    In    q,
    file  f,
    float b,
)
`).TypeTable)
}

// TestApplyKeepsObjectOrder checks the file-leaf walk rewrites leaves inside
// Objects without reordering them, while a typed map's leaves are still
// visited in sorted key order.
func TestApplyKeepsObjectOrder(t *testing.T) {
	tbl := orderTable(t)
	params := fields(t, "box Box", "fm map<file>")
	vals := map[string]any{
		"box": decodeOrdered(t, `{"b":1.0,"f":"x.txt","q":{"a":1,"z":2},"extra":"e.txt"}`),
		"fm":  decodeOrdered(t, `{"k2":"two","k1":"one"}`),
	}

	fn, seen := collector()

	got, err := tbl.Apply(params, vals, func(s string) (string, error) {
		_, _ = fn(s)

		return strings.ToUpper(s), nil
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := `{"box":{"b":1.0,"f":"X.TXT","q":{"a":1,"z":2},"extra":"e.txt"},"fm":{"k2":"TWO","k1":"ONE"}}`
	if enc := encode(t, got); enc != want {
		t.Errorf("Apply = %s, want %s", enc, want)
	}

	if diff := cmp.Diff([]string{"x.txt", "one", "two"}, *seen); diff != "" {
		t.Errorf("leaf visit order (-want +got):\n%s", diff)
	}
}
