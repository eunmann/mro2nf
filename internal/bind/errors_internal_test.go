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

// TestResolveForksArraySplitErrors covers buildArrayForks' failure arms:
// zipped split arrays of mismatched lengths and a non-array split source.
func TestResolveForksArraySplitErrors(t *testing.T) {
	tests := []struct {
		name     string
		pipeArgs string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := Spec{
				"x": {Ref: &Ref{Kind: "self", ID: "xs"}, Split: true},
				"y": {Ref: &Ref{Kind: "self", ID: "ys"}, Split: true},
			}

			_, _, err := ResolveForks(spec, json.RawMessage(tt.pipeArgs), nil, false)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ResolveForks error = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

// TestResolveForksMapSplitErrors covers buildMapForks' failure arms: an array
// split source in map mode and two map splits with different key sets (which
// also exercises equalKeys).
func TestResolveForksMapSplitErrors(t *testing.T) {
	tests := []struct {
		name     string
		pipeArgs string
		wantErr  error
	}{
		{
			name:     "array split binding in map mode",
			pipeArgs: `{"xs":[1,2],"ys":[1,2]}`,
			wantErr:  errNotMap,
		},
		{
			name:     "mismatched map key sets",
			pipeArgs: `{"xs":{"a":1,"b":2},"ys":{"a":1,"c":3}}`,
			wantErr:  errSplitLen,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := Spec{
				"x": {Ref: &Ref{Kind: "self", ID: "xs"}, Split: true},
				"y": {Ref: &Ref{Kind: "self", ID: "ys"}, Split: true},
			}

			_, _, err := ResolveForks(spec, json.RawMessage(tt.pipeArgs), nil, true)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ResolveForks error = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

// TestResolveForksNullMapSource verifies the {} side of the null-source merge
// contract: a null typed-map split still forks as a map with zero forks and a
// NON-nil empty key slice, so Merge yields {} (not [] and not null).
func TestResolveForksNullMapSource(t *testing.T) {
	spec := Spec{"m": {Ref: &Ref{Kind: "self", ID: "src"}, Split: true}}

	forks, keys, err := ResolveForks(spec, json.RawMessage(`{"src":null}`), nil, true)
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

	merged, err := Merge([]string{"o"}, nil, keys)
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

	forks, keys, err := ResolveForks(spec, json.RawMessage(`{"src":null}`), nil, false)
	if err != nil {
		t.Fatalf("resolve forks: %v", err)
	}

	if len(forks) != 0 {
		t.Errorf("forks = %d, want 0", len(forks))
	}
	if keys != nil {
		t.Errorf("keys = %v, want nil for array mode", keys)
	}

	merged, err := Merge([]string{"o"}, nil, keys)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := string(merged); got != `{"o":[]}` {
		t.Errorf("merged = %s, want {\"o\":[]}", got)
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

// TestMergeOutsKeysDesync covers the outs/keys length guard in mergeOne: a
// desync fails with errSplitLen and an error naming both counts.
func TestMergeOutsKeysDesync(t *testing.T) {
	outs := []json.RawMessage{
		json.RawMessage(`{"w":1}`),
		json.RawMessage(`{"w":2}`),
	}

	_, err := Merge([]string{"w"}, outs, []string{"a", "b", "c"})
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
