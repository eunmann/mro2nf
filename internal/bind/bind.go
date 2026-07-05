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
	"slices"
	"sort"
	"strings"
)

var (
	// errUnknownRefKind is returned for a reference that is neither self nor call.
	errUnknownRefKind = errors.New("unknown ref kind")
	// errNoSplit is returned when a fork is requested with no split binding.
	errNoSplit = errors.New("map call has no split binding")
	// errMultiSplit is returned when an element resolve sees several split
	// bindings; a single pre-sliced element cannot represent a zip.
	errMultiSplit = errors.New("element resolve requires exactly one split binding")
	// errSplitLen is returned when zipped split collections differ in length.
	errSplitLen = errors.New("split collections have mismatched lengths")
	// errNotArray is returned when an array-mode split binding is not an array.
	errNotArray = errors.New("split binding is not an array")
	// errNotMap is returned when a map-mode split binding is not a typed map.
	errNotMap = errors.New("split binding is not a map")
)

// Ref is a reference to a pipeline input (self) or an upstream call output.
type Ref struct {
	// Kind is "self" or "call".
	Kind string `json:"kind"`
	// ID is the pipeline input name (self) or call instance id (call).
	ID string `json:"id"`
	// Output is the dotted projection within the referent (empty = whole value).
	Output string `json:"output"`
	// MapDepth, when > 0, marks a field projection through a typed map: navigate
	// the first MapDepth segments of the path, then project the remainder over
	// the values of the typed map reached there (map<S>.field -> map<field>).
	// The emitter computes it from the program's types. 0 means no map
	// projection (arrays auto-project at runtime; structs navigate by key).
	MapDepth int `json:"mapDepth,omitempty"`
	// MapInArray marks the array<map<S>>.field shape: the value reached at
	// MapDepth is an array (one or more dims) of typed maps, so the field is
	// projected over each map's values *within* the array, preserving the array
	// structure (array<map<S>>.field -> array<map<field>>).
	MapInArray bool `json:"mapInArray,omitempty"`
}

// Entry binds one parameter to a value expression: a leaf literal or ref, or a
// composite array/object whose elements may contain refs (fan-in). Exactly one
// of Literal/Ref/Array/Object is set.
type Entry struct {
	Literal json.RawMessage  `json:"literal,omitempty"`
	Ref     *Ref             `json:"ref,omitempty"`
	Array   []Entry          `json:"array,omitempty"`
	Object  map[string]Entry `json:"object,omitempty"`
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

// ResolveElement builds one fork's _args JSON from a PRE-SLICED split element,
// skipping the whole-collection parse a per-fork ResolveForks would do (the
// native-scatter O(N^2) fix, #99): the driver extracts element i once and each
// fork instance assembles its args in O(1). Every split binding takes element
// verbatim; broadcast bindings resolve against pipeArgs/callOuts as usual. A
// map call has exactly one split binding on the native-scatter path, so element
// is that one param's value; the caller guarantees element matches the split
// param's element type. A spec with zero split bindings is rejected with the
// same errNoSplit ResolveForks reports, and several split bindings are
// rejected too — one element cannot stand in for a zip of collections.
func ResolveElement(spec Spec, pipeArgs json.RawMessage, callOuts map[string]json.RawMessage, element json.RawMessage) (json.RawMessage, error) {
	args := make(map[string]json.RawMessage, len(spec))
	splits := 0

	for param, entry := range spec {
		if entry.Split {
			args[param] = element
			splits++

			continue
		}

		val, err := entry.resolve(pipeArgs, callOuts)
		if err != nil {
			return nil, fmt.Errorf("bind %q: %w", param, err)
		}

		args[param] = val
	}

	if splits == 0 {
		return nil, errNoSplit
	}

	if splits > 1 {
		return nil, fmt.Errorf("%w: got %d", errMultiSplit, splits)
	}

	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal element args: %w", err)
	}

	return raw, nil
}

// AllForks marks every fork's args for marshaling in ResolveForks.
const AllForks = -1

// ResolveForks resolves a map call's bindings into one _args object per fork.
// Split bindings are resolved to collections and zipped element-wise; non-split
// bindings are broadcast to every fork. isMap selects the fork kind from the
// call's STATIC map mode (array vs typed map) rather than sniffing the runtime
// value — so a null or empty typed-map source still forks as a map (keys nil-
// safe) and merges to {}, and a null/empty array source merges to [], matching
// Martian's type-directed dispatch. For a map fork the returned keys give each
// fork's map key (sorted); for an array fork keys is nil. `only` selects which
// fork's args to marshal (AllForks marshals every fork; other slots stay nil):
// resolution and validation are identical either way — same collection, zip,
// and mode checks, same fork count, same keys — so a single native-scatter
// instance (#76) pays one args marshal instead of N while failing exactly
// where the full FORK-task resolve would.
func ResolveForks(spec Spec, pipeArgs json.RawMessage, callOuts map[string]json.RawMessage, isMap bool, only int) ([]json.RawMessage, []string, error) {
	broadcast := map[string]json.RawMessage{}
	splits := map[string]json.RawMessage{}

	for param, entry := range spec {
		val, err := entry.resolve(pipeArgs, callOuts)
		if err != nil {
			return nil, nil, fmt.Errorf("bind %q: %w", param, err)
		}

		if entry.Split {
			splits[param] = val
		} else {
			broadcast[param] = val
		}
	}

	if len(splits) == 0 {
		return nil, nil, errNoSplit
	}

	if isMap {
		return buildMapForks(broadcast, splits, only)
	}

	return buildArrayForks(broadcast, splits, only)
}

func buildArrayForks(broadcast, splits map[string]json.RawMessage, only int) ([]json.RawMessage, []string, error) {
	arrays := make(map[string][]json.RawMessage, len(splits))
	count := -1

	for param, raw := range splits {
		var arr []json.RawMessage
		if err := json.Unmarshal(orEmptyArray(raw), &arr); err != nil {
			return nil, nil, fmt.Errorf("split %q: %w", param, errNotArray)
		}

		if count == -1 {
			count = len(arr)
		} else if len(arr) != count {
			return nil, nil, fmt.Errorf("split %q: %w", param, errSplitLen)
		}

		arrays[param] = arr
	}

	forks := make([]json.RawMessage, count)

	for i := range count {
		if only != AllForks && i != only {
			continue
		}

		args := make(map[string]json.RawMessage, len(broadcast)+len(arrays))
		maps.Copy(args, broadcast)

		for param, arr := range arrays {
			args[param] = arr[i]
		}

		raw, err := json.Marshal(args)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal fork %d: %w", i, err)
		}

		forks[i] = raw
	}

	return forks, nil, nil
}

func buildMapForks(broadcast, splits map[string]json.RawMessage, only int) ([]json.RawMessage, []string, error) {
	maps0 := make(map[string]map[string]json.RawMessage, len(splits))
	keys := []string(nil)

	for param, raw := range splits {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, nil, fmt.Errorf("split %q: %w", param, errNotMap)
		}

		ks := sortedKeys(m)
		if keys == nil {
			keys = ks
		} else if !equalKeys(keys, ks) {
			return nil, nil, fmt.Errorf("split %q: %w", param, errSplitLen)
		}

		maps0[param] = m
	}

	forks := make([]json.RawMessage, len(keys))

	for i, k := range keys {
		if only != AllForks && i != only {
			continue
		}

		args := make(map[string]json.RawMessage, len(broadcast)+len(maps0))
		maps.Copy(args, broadcast)

		for param, m := range maps0 {
			args[param] = m[k]
		}

		raw, err := json.Marshal(args)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal fork %q: %w", k, err)
		}

		forks[i] = raw
	}

	return forks, keys, nil
}

// Merge combines per-fork outputs into a single map-call result. For an array
// fork (keys nil), each named output becomes an array of that field across the
// forks in order; for a map fork, each output becomes a map keyed by keys[i].
// emptyNull applies mrp's invocation-known-empty rule (#99): with ZERO forks
// every output merges to null instead of the typed empty — the emitter sets it
// only for a map call whose split source is launch-invocation-known
// (entrySplit), where mrp's static resolver would prune the zero-fork call and
// resolve its outputs to null. A runtime-sourced empty keeps the typed empty.
func Merge(names []string, outs []json.RawMessage, keys []string, emptyNull bool) (json.RawMessage, error) {
	// The zero-fork null decision is made once, before the per-name loop, and
	// only after the same keys/outs desync guard mergeOne applies — so the
	// precedence is structural: desync error > null > typed value.
	zeroNull := emptyNull && len(outs) == 0

	if zeroNull && keys != nil && len(keys) != 0 {
		return nil, fmt.Errorf("merge: 0 fork outputs for %d keys: %w", len(keys), errSplitLen)
	}

	result := make(map[string]json.RawMessage, len(names))

	for _, name := range names {
		if zeroNull {
			result[name] = json.RawMessage("null")

			continue
		}

		raw, err := mergeOne(name, outs, keys)
		if err != nil {
			return nil, err
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
	switch {
	case e.Array != nil:
		out := make([]json.RawMessage, 0, len(e.Array))

		for _, el := range e.Array {
			v, err := el.resolve(pipeArgs, callOuts)
			if err != nil {
				return nil, err
			}

			out = append(out, v)
		}

		return marshalRaw(out, "array")
	case e.Object != nil:
		out := make(map[string]json.RawMessage, len(e.Object))

		for k, el := range e.Object {
			v, err := el.resolve(pipeArgs, callOuts)
			if err != nil {
				return nil, err
			}

			out[k] = v
		}

		return marshalRaw(out, "object")
	case e.Literal != nil:
		return e.Literal, nil
	case e.Ref == nil:
		return json.RawMessage(nullLiteral), nil
	}

	switch e.Ref.Kind {
	case "self":
		return extractProject(pipeArgs, joinPath(e.Ref.ID, e.Ref.Output), e.Ref.MapDepth, e.Ref.MapInArray)
	case "call":
		outs, ok := callOuts[e.Ref.ID]
		if !ok {
			return json.RawMessage(nullLiteral), nil
		}

		return extractProject(outs, e.Ref.Output, e.Ref.MapDepth, e.Ref.MapInArray)
	default:
		return nil, fmt.Errorf("%w: %q", errUnknownRefKind, e.Ref.Kind)
	}
}

// extractProject navigates the first mapDepth segments, then projects the rest
// of the path over the values of the typed map reached there. With mapDepth <= 0
// it is the plain navigate/array-project extract. When mapInArray is set the
// value at mapDepth is an array of maps, so the projection descends the array and
// projects over each map (array<map<S>>.field -> array<map<field>>).
func extractProject(raw json.RawMessage, path string, mapDepth int, mapInArray bool) (json.RawMessage, error) {
	if mapDepth <= 0 && !mapInArray {
		return extract(raw, path)
	}

	segs := strings.Split(path, ".")
	if mapDepth >= len(segs) {
		return extract(raw, path)
	}

	mapVal, err := extract(raw, strings.Join(segs[:mapDepth], "."))
	if err != nil {
		return nil, err
	}

	if mapInArray {
		return projectMapInArray(mapVal, strings.Join(segs[mapDepth:], "."))
	}

	return projectMap(mapVal, strings.Join(segs[mapDepth:], "."))
}

// projectMapInArray projects path over the typed-map values nested inside an
// array (any depth): it recurses through array levels and applies projectMap at
// each map, so array<map<S>>.field yields array<map<field>> and the array shape
// is preserved.
func projectMapInArray(raw json.RawMessage, path string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte(nullLiteral)) {
		return json.RawMessage(nullLiteral), nil
	}

	if trimmed[0] != '[' {
		return projectMap(raw, path)
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("project %q over array-of-map: %w", path, err)
	}

	out := make([]json.RawMessage, len(arr))
	for i, e := range arr {
		pv, err := projectMapInArray(e, path)
		if err != nil {
			return nil, err
		}

		out[i] = pv
	}

	return marshalRaw(out, "array-of-map projection")
}

// projectMap applies path to each value of a typed-map object, returning a map
// of the projected values (null-tolerant for a null/absent map).
func projectMap(raw json.RawMessage, path string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte(nullLiteral)) {
		return json.RawMessage(nullLiteral), nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("project %q over map: %w", path, err)
	}

	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		pv, err := extract(v, path)
		if err != nil {
			return nil, err
		}

		out[k] = pv
	}

	return marshalRaw(out, "map projection")
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

// mergeOne computes one output's merged value: a key→value map for a map fork
// (empty map for zero forks), or an ordered array for an array fork (empty array
// for zero forks).
func mergeOne(name string, outs []json.RawMessage, keys []string) (json.RawMessage, error) {
	if keys != nil {
		// The keys (forkkeys.json) and outs (collected per-fork bundles) are
		// produced independently; a desync would index outs out of range. Fail
		// with a clear error rather than panicking the merge process.
		if len(outs) != len(keys) {
			return nil, fmt.Errorf("merge %q: %d fork outputs for %d keys: %w", name, len(outs), len(keys), errSplitLen)
		}

		m := make(map[string]json.RawMessage, len(keys))

		for i, k := range keys {
			v, err := extract(outs[i], name)
			if err != nil {
				return nil, fmt.Errorf("merge %q: %w", name, err)
			}

			m[k] = v
		}

		return marshalRaw(m, name)
	}

	// An array-mode fork (keys nil) with zero forks resolves to an empty array,
	// not null — Martian's runtime merge yields marshallerArray{} ([]) for an
	// empty typed-array source (and for a null one, since the type is known). A
	// statically-known-empty literal is collapsed to null earlier by Martian's
	// compiler; Merge's emptyNull reproduces that for invocation-known split
	// sources (#99) while this default keeps the runtime typed empty.
	if len(outs) == 0 {
		return json.RawMessage("[]"), nil
	}

	arr := make([]json.RawMessage, 0, len(outs))

	for _, out := range outs {
		v, err := extract(out, name)
		if err != nil {
			return nil, fmt.Errorf("merge %q: %w", name, err)
		}

		arr = append(arr, v)
	}

	return marshalRaw(arr, name)
}

func marshalRaw(v any, name string) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("merge %q: %w", name, err)
	}

	return raw, nil
}

func orEmptyArray(raw json.RawMessage) json.RawMessage {
	if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == nullLiteral {
		return json.RawMessage("[]")
	}

	return raw
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func equalKeys(a, b []string) bool {
	return slices.Equal(a, b)
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
