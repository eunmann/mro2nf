package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eunmann/mro2nf/internal/types"
)

// errNotObject is returned when a payload that should be a JSON object isn't.
var errNotObject = errors.New("expected a JSON object")

// rawToMap decodes a JSON object into a map of its values, each decoded by
// types.DecodeJSON, so a number keeps its token (42.0 stays 42.0) and a nested
// object its key order across the bundle round-trip; the top level is written
// back with its keys sorted, as mrp writes every _args and _outs. An empty
// payload, JSON null, or the empty string the Martian adapter writes for a
// stage with no outputs yields an empty map. Any other non-object payload (an
// array, a number, or corrupt JSON) is an error, so genuinely malformed outputs
// surface instead of silently becoming empty.
func rawToMap(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		return out, nil
	}

	if trimmed[0] != '{' {
		return nil, fmt.Errorf("decode payload: %w, got %.32s", errNotObject, trimmed)
	}

	v, err := types.DecodeJSON(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	obj, _ := types.AsObject(v)
	for _, m := range obj {
		out[m.Key] = m.Value
	}

	return out, nil
}
