// Package bind resolves a call's input bindings into a concrete _args object at
// runtime, combining literal values, pipeline inputs (self.X), and the outputs
// of upstream calls (CALL.field).
package bind

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// errUnknownRefKind is returned for a reference that is neither self nor call.
var errUnknownRefKind = errors.New("unknown ref kind")

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
func extract(raw json.RawMessage, path string) (json.RawMessage, error) {
	if path == "" || len(raw) == 0 {
		return orNull(raw), nil
	}

	cur := raw

	for key := range strings.SplitSeq(path, ".") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			return nil, fmt.Errorf("navigate %q: %w", key, err)
		}

		next, ok := obj[key]
		if !ok {
			return json.RawMessage(nullLiteral), nil
		}

		cur = next
	}

	return cur, nil
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
