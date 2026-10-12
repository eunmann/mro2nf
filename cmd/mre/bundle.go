package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/eunmann/mro2nf/internal/types"
)

// errNotObject is returned when a payload that should be a JSON object isn't.
var errNotObject = errors.New("expected a JSON object")

// producer holds the flags a subcommand needs to type a bundle's values: the
// type manifest, the producing callable, and which of its parameter sets
// applies.
type producer struct {
	types    string
	callable string
	role     string
}

func addProducer(fs *flag.FlagSet, defaultRole string) *producer {
	p := &producer{}
	fs.StringVar(&p.types, "types", "", "type manifest (types.json) path")
	fs.StringVar(&p.callable, "callable", "", "producing callable name")
	fs.StringVar(&p.role, "role", defaultRole, "param set: in|out|mainout|chunkin")

	return p
}

// manifest loads the type manifest, or an empty one when no path is configured
// (e.g. pipelines with no file outputs need no rewriting).
func (p *producer) manifest() (types.Manifest, error) {
	if p.types == "" {
		return types.Manifest{}, nil
	}

	man, err := types.LoadManifest(p.types)
	if err != nil {
		return types.Manifest{}, fmt.Errorf("load type manifest: %w", err)
	}

	return man, nil
}

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
