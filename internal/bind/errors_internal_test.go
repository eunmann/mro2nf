// In-package tests for bind's error sentinels and null/empty edge behavior:
// these exercise unexported sentinels (errSplitLen, errNotArray, errNotMap,
// errUnknownRefKind) via errors.Is and the internal helpers directly.

package bind

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestResolveForksSplitErrors covers buildArrayForks' and buildMapForks'
// failure arms: zipped split collections of mismatched lengths/key sets
// (which also exercises equalKeys) and a wrong-kind split source under each
// mode.
func TestResolveForksSplitErrors(t *testing.T) {
	tests := []struct {
		name     string
		pipeArgs string
		isMap    bool
		wantErr  error
	}{
		{
			name:     "mismatched split lengths",
			pipeArgs: `{"xs":[1,2,3],"ys":[10,20]}`,
			wantErr:  errSplitLen,
		},
		{
			name:     "object split binding in array mode",
			pipeArgs: `{"xs":{"a":1},"ys":{"a":1}}`,
			wantErr:  errNotArray,
		},
		{
			name:     "array split binding in map mode",
			pipeArgs: `{"xs":[1,2],"ys":[1,2]}`,
			isMap:    true,
			wantErr:  errNotMap,
		},
		{
			// Same length, different keys: a KEYS error, not the misleading
			// "mismatched lengths" (#187).
			name:     "mismatched map key sets",
			pipeArgs: `{"xs":{"a":1,"b":2},"ys":{"a":1,"c":3}}`,
			isMap:    true,
			wantErr:  errSplitKeys,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := Spec{
				"x": {Ref: &Ref{Kind: "self", ID: "xs"}, Split: true},
				"y": {Ref: &Ref{Kind: "self", ID: "ys"}, Split: true},
			}

			// The single-fork marshal arm (only=0, the native-scatter -index
			// path) must validate exactly like the full resolve.
			for _, only := range []int{AllForks, 0} {
				_, _, err := ResolveForks(spec, json.RawMessage(tt.pipeArgs), nil, tt.isMap, only)
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("ResolveForks(only=%d) error = %v, want errors.Is %v", only, err, tt.wantErr)
				}
			}
		})
	}
}

// TestResolveForksNullMapSource verifies the {} side of the null-source merge
// contract: a null typed-map split still forks as a map with zero forks and a
// NON-nil empty key slice, so Merge yields {} (not [] and not null).
func TestResolveForksNullMapSource(t *testing.T) {
	spec := Spec{"m": {Ref: &Ref{Kind: "self", ID: "src"}, Split: true}}

	forks, keys, err := ResolveForks(spec, json.RawMessage(`{"src":null}`), nil, true, AllForks)
	if err != nil {
		t.Fatalf("resolve forks: %v", err)
	}

	if len(forks) != 0 {
		t.Errorf("forks = %d, want 0", len(forks))
	}
	if keys == nil {
		t.Fatalf("keys = nil for null map source, want non-nil empty slice (nil keys would merge to [] instead of {})")
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want empty", keys)
	}

	merged, err := Merge([]string{"o"}, nil, keys, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := string(merged); got != `{"o":{}}` {
		t.Errorf("merged = %s, want {\"o\":{}}", got)
	}
}

// TestResolveForksNullArraySource verifies the [] side of the contract: a null
// array split forks to zero forks with nil keys, and Merge yields [].
func TestResolveForksNullArraySource(t *testing.T) {
	spec := Spec{"x": {Ref: &Ref{Kind: "self", ID: "src"}, Split: true}}

	forks, keys, err := ResolveForks(spec, json.RawMessage(`{"src":null}`), nil, false, AllForks)
	if err != nil {
		t.Fatalf("resolve forks: %v", err)
	}

	if len(forks) != 0 {
		t.Errorf("forks = %d, want 0", len(forks))
	}
	if keys != nil {
		t.Errorf("keys = %v, want nil for array mode", keys)
	}

	merged, err := Merge([]string{"o"}, nil, keys, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := string(merged); got != `{"o":[]}` {
		t.Errorf("merged = %s, want {\"o\":[]}", got)
	}
}

// TestResolveElementSplitCount checks ResolveElement rejects the spec shapes
// the native-scatter path cannot represent: no split binding (errNoSplit, as
// ResolveForks reports) and more than one split binding (a single pre-sliced
// element cannot stand in for a zip of several collections).
func TestResolveElementSplitCount(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantErr error
	}{
		{
			name:    "no split binding",
			spec:    Spec{"x": {Ref: &Ref{Kind: "self", ID: "xs"}}},
			wantErr: errNoSplit,
		},
		{
			name: "multiple split bindings",
			spec: Spec{
				"x": {Ref: &Ref{Kind: "self", ID: "xs"}, Split: true},
				"y": {Ref: &Ref{Kind: "self", ID: "ys"}, Split: true},
			},
			wantErr: errMultiSplit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipeArgs := json.RawMessage(`{"xs":[1],"ys":[2]}`)

			_, err := ResolveElement(tt.spec, pipeArgs, nil, json.RawMessage(`1`))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ResolveElement error = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

// TestEntryResolveUnknownRefKind hits the default arm of Entry.resolve's ref
// dispatch, both directly and wrapped through the public Resolve.
func TestEntryResolveUnknownRefKind(t *testing.T) {
	entry := Entry{Ref: &Ref{Kind: "bogus", ID: "x"}}

	if _, err := entry.resolve(nil, nil); !errors.Is(err, errUnknownRefKind) {
		t.Errorf("resolve error = %v, want errors.Is errUnknownRefKind", err)
	}

	_, err := Resolve(Spec{"p": entry}, nil, nil)
	if !errors.Is(err, errUnknownRefKind) {
		t.Errorf("Resolve error = %v, want errors.Is errUnknownRefKind", err)
	}
	if err == nil || !strings.Contains(err.Error(), `bind "p"`) {
		t.Errorf("Resolve error = %v, want param context %q", err, `bind "p"`)
	}
}

// TestResolveMarshalErrorNamesBinding checks a marshal failure inside binding
// resolution reports the binding operation, not "merge" (marshalRaw used to
// stamp every failure as a merge regardless of the caller's actual operation).
func TestResolveMarshalErrorNamesBinding(t *testing.T) {
	// A corrupt RawMessage literal survives resolve verbatim and fails only at
	// the composite marshal.
	spec := Spec{"p": {Array: []Entry{{Literal: json.RawMessage(`{bad`)}}}}

	_, err := Resolve(spec, nil, nil)
	if err == nil {
		t.Fatal("Resolve with corrupt literal: want marshal error, got nil")
	}

	if strings.Contains(err.Error(), "merge") {
		t.Errorf("binding-resolution marshal error mentions merge: %v", err)
	}

	if !strings.Contains(err.Error(), "marshal array binding") {
		t.Errorf("error = %v, want the actual operation %q", err, "marshal array binding")
	}
}

// TestMergeOutsKeysDesync covers the outs/keys length guard in mergeOne: a
// desync fails with errSplitLen and an error naming both counts.
func TestMergeOutsKeysDesync(t *testing.T) {
	outs := []json.RawMessage{
		json.RawMessage(`{"w":1}`),
		json.RawMessage(`{"w":2}`),
	}

	_, err := Merge([]string{"w"}, outs, []string{"a", "b", "c"}, false)
	if !errors.Is(err, errSplitLen) {
		t.Fatalf("Merge error = %v, want errors.Is errSplitLen", err)
	}
	if !strings.Contains(err.Error(), "2 fork outputs for 3 keys") {
		t.Errorf("Merge error = %v, want counts \"2 fork outputs for 3 keys\"", err)
	}
}

// TestExtractScalarMidPath covers extract's navigate arm failing when a path
// segment lands on a scalar that cannot be unmarshalled as an object.
func TestExtractScalarMidPath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		path string
	}{
		{name: "number under two-segment path", raw: `5`, path: "a.b"},
		{name: "string under one-segment path", raw: `"str"`, path: "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := extract(json.RawMessage(tt.raw), tt.path)
			if err == nil {
				t.Fatalf("extract(%s, %q) = nil error, want navigate failure", tt.raw, tt.path)
			}
			if !strings.Contains(err.Error(), "navigate") {
				t.Errorf("extract error = %v, want navigate context", err)
			}
		})
	}
}

// TestProjectMapInArray covers projectMapInArray's shape handling: null
// passthrough, deep array nesting preserved, and non-array delegation to
// projectMap.
func TestProjectMapInArray(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		path string
		want string
	}{
		{name: "null source", raw: `null`, path: "x", want: `null`},
		{name: "empty source", raw: ``, path: "x", want: `null`},
		{
			name: "deep array nesting preserved",
			raw:  `[[{"a":{"x":1},"b":{"x":2}}],[{"c":{"x":3}}]]`,
			path: "x",
			want: `[[{"a":1,"b":2}],[{"c":3}]]`,
		},
		{
			name: "non-array delegates to projectMap",
			raw:  `{"a":{"x":5}}`,
			path: "x",
			want: `{"a":5}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := projectMapInArray(json.RawMessage(tt.raw), tt.path)
			if err != nil {
				t.Fatalf("projectMapInArray: %v", err)
			}

			if diff := cmp.Diff(tt.want, string(got)); diff != "" {
				t.Errorf("projection mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOrEmptyArray covers the null/empty-to-[] normalization used by array
// split resolution and passthrough of anything else.
func TestOrEmptyArray(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty raw", raw: ``, want: `[]`},
		{name: "null literal", raw: `null`, want: `[]`},
		{name: "null with whitespace", raw: `  null `, want: `[]`},
		{name: "array passthrough", raw: `[1,2]`, want: `[1,2]`},
		{name: "object passthrough", raw: `{"a":1}`, want: `{"a":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(orEmptyArray(json.RawMessage(tt.raw))); got != tt.want {
				t.Errorf("orEmptyArray(%q) = %s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}

// TestExtractProjectArrayOfMapField pins the runtime contract the #172 emit fix
// relies on: given the (mapDepth 2, mapInArray) the emitter now computes for
// arr.m.x, the binder projects over each map in the array and yields
// array<map<field>>, matching mrp — not the [null,null] a depth-0 key-navigation
// produces. (The binder already supported this; the emit fix is what asks for
// it, so this is a characterization guard on the depth->result contract.) The
// collision row also pins that a map key equal to the projected field name (x)
// survives as the key rather than being confused with the field access.
func TestExtractProjectArrayOfMapField(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "distinct keys",
			raw:  `{"arr":[{"m":{"k":{"x":1}}},{"m":{"j":{"x":2}}}]}`,
			want: `[{"k":1},{"j":2}]`,
		},
		{
			// The map key equals the projected field name: the key must be kept,
			// not conflated with the .x navigation.
			name: "key collides with field name",
			raw:  `{"arr":[{"m":{"x":{"x":99},"y":{"x":7}}}]}`,
			want: `[{"x":99,"y":7}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractProject(json.RawMessage(tc.raw), "arr.m.x", 2, true)
			if err != nil {
				t.Fatalf("extractProject: %v", err)
			}

			var gotVal, wantVal any
			if err := json.Unmarshal(got, &gotVal); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantVal); err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(wantVal, gotVal); diff != "" {
				t.Errorf("array<map>.field projection mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
