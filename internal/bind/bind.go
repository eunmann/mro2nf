// Package bind resolves a call's input bindings into a concrete _args object at
// runtime, combining literal values, pipeline inputs (self.X), and the outputs
// of upstream calls (CALL.field).
package bind

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
)

var (
	// errUnknownRefKind is returned for a reference that is neither self nor call.
	errUnknownRefKind = errors.New("unknown ref kind")
	// errNoSplit is returned when a fork is requested with no split binding.
	errNoSplit = errors.New("map call has no split binding")
	// errSplitLen is returned when zipped split collections differ in length.
	errSplitLen = errors.New("split collections have mismatched lengths")
	// errNotArray is returned when a split binding is not bound to an array.
	errNotArray = errors.New("split binding is not an array")
)

// Ref is a reference to a pipeline input (self) or an upstream call output.
type Ref struct {
	// Kind is "self" or "call".
	Kind string `json:"kind"`
	// ID is the pipeline input name (self) or call instance id (call).
	ID string `json:"id"`
	// Output is the dotted projection within the referent (empty = whole value).
	Output string `json:"output"`
}

// Entry binds one parameter: exactly one of Literal or Ref is set.
type Entry struct {
	Literal json.RawMessage `json:"literal,omitempty"`
	Ref     *Ref            `json:"ref,omitempty"`
	// Split marks a map-call fork dimension: the resolved value is a
	// collection iterated one element per fork.
	Split bool `json:"split,omitempty"`
}

// Spec maps each callee parameter name to its binding.
type Spec map[string]Entry

// Resolve builds the _args JSON for a call. pipeArgs is the enclosing
// pipeline's input args (for self refs); callOuts maps an upstream call id to
// its _outs JSON (for call refs).
func Resolve(spec Spec, pipeArgs json.RawMessage, callOuts map[string]json.RawMessage) (json.RawMessage, error) {
	args := make(map[string]json.RawMessage, len(spec))

	for param, entry := range spec {
		val, err := entry.resolve(pipeArgs, callOuts)
		if err != nil {
			return nil, fmt.Errorf("bind %q: %w", param, err)
		}

		args[param] = val
	}

	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal args: %w", err)
	}

	return raw, nil
}

// ResolveForks resolves a map call's bindings into one _args object per fork.
// Split bindings are resolved to collections and zipped element-wise; non-split
// bindings are broadcast to every fork.
func ResolveForks(spec Spec, pipeArgs json.RawMessage, callOuts map[string]json.RawMessage) ([]json.RawMessage, error) {
	broadcast := map[string]json.RawMessage{}
	splits := map[string][]json.RawMessage{}
	count := -1

	for param, entry := range spec {
		val, err := entry.resolve(pipeArgs, callOuts)
		if err != nil {
			return nil, fmt.Errorf("bind %q: %w", param, err)
		}

		if !entry.Split {
			broadcast[param] = val

			continue
		}

		var arr []json.RawMessage
		if err := json.Unmarshal(val, &arr); err != nil {
			return nil, fmt.Errorf("split %q: %w", param, errNotArray)
		}

		if count == -1 {
			count = len(arr)
		} else if len(arr) != count {
			return nil, fmt.Errorf("split %q: %w", param, errSplitLen)
		}

		splits[param] = arr
	}

	if count == -1 {
		return nil, errNoSplit
	}

	return buildForks(broadcast, splits, count)
}

func buildForks(broadcast map[string]json.RawMessage, splits map[string][]json.RawMessage, count int) ([]json.RawMessage, error) {
	forks := make([]json.RawMessage, 0, count)

	for i := range count {
		args := make(map[string]json.RawMessage, len(broadcast)+len(splits))
		maps.Copy(args, broadcast)

		for param, arr := range splits {
			args[param] = arr[i]
		}

		raw, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("marshal fork %d: %w", i, err)
		}

		forks = append(forks, raw)
	}

	return forks, nil
}

// Merge combines per-fork outputs into a single map-call result: each named
// output becomes an array of that field across the forks, in order.
func Merge(names []string, outs []json.RawMessage) (json.RawMessage, error) {
	result := make(map[string]json.RawMessage, len(names))

	for _, name := range names {
		// An empty map call resolves to null per output, matching Martian's
		// null-map-call semantics (not an empty array).
		if len(outs) == 0 {
			result[name] = json.RawMessage(nullLiteral)

			continue
		}

		arr := make([]json.RawMessage, 0, len(outs))

		for _, out := range outs {
			val, err := extract(out, name)
			if err != nil {
				return nil, fmt.Errorf("merge %q: %w", name, err)
			}

			arr = append(arr, val)
		}

		raw, err := json.Marshal(arr)
		if err != nil {
			return nil, fmt.Errorf("merge %q: %w", name, err)
		}

		result[name] = raw
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}

	return raw, nil
}

func (e Entry) resolve(pipeArgs json.RawMessage, callOuts map[string]json.RawMessage) (json.RawMessage, error) {
	if e.Literal != nil {
		return e.Literal, nil
	}

	if e.Ref == nil {
		return json.RawMessage(nullLiteral), nil
	}

	switch e.Ref.Kind {
	case "self":
		return extract(pipeArgs, joinPath(e.Ref.ID, e.Ref.Output))
	case "call":
		outs, ok := callOuts[e.Ref.ID]
		if !ok {
			return json.RawMessage(nullLiteral), nil
		}

		return extract(outs, e.Ref.Output)
	default:
		return nil, fmt.Errorf("%w: %q", errUnknownRefKind, e.Ref.Kind)
	}
}

const nullLiteral = "null"

// extract navigates a JSON value along a dotted key path. A missing key
// resolves to null, mirroring how Martian treats a disabled upstream output.
// When the value at a step is an array, the remaining path is projected over
// each element (Martian projects field access through arrays), so e.g.
// CALL.s.mean over {"s":[{"mean":1},{"mean":2}]} yields [1,2].
func extract(raw json.RawMessage, path string) (json.RawMessage, error) {
	if path == "" || len(raw) == 0 {
		return orNull(raw), nil
	}

	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		return projectArray(raw, path)
	}

	key, rest, _ := strings.Cut(path, ".")

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("navigate %q: %w", key, err)
	}

	next, ok := obj[key]
	if !ok {
		return json.RawMessage(nullLiteral), nil
	}

	return extract(next, rest)
}

// projectArray applies the path to each element of a JSON array and returns the
// array of results.
func projectArray(raw json.RawMessage, path string) (json.RawMessage, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, fmt.Errorf("project %q over array: %w", path, err)
	}

	out := make([]json.RawMessage, 0, len(elems))

	for _, elem := range elems {
		v, err := extract(elem, path)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal projection: %w", err)
	}

	return raw, nil
}

func joinPath(id, output string) string {
	if output == "" {
		return id
	}

	return id + "." + output
}

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(nullLiteral)
	}

	return raw
}
