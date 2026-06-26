package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/eunmann/martian-nextflow/internal/shim"
	"github.com/eunmann/martian-nextflow/internal/types"
)

// errNotObject is returned when a payload that should be a JSON object isn't.
var errNotObject = errors.New("expected a JSON object")

// producer holds the flags a bundle-writing subcommand needs to locate the file
// leaves it must stage: the type manifest, the producing callable, and which of
// its parameter sets applies.
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

// write serializes raw as a bundle directory, staging its file leaves.
func (p *producer) write(dir string, raw json.RawMessage) error {
	man, err := p.manifest()
	if err != nil {
		return err
	}

	payload, err := rawToMap(raw)
	if err != nil {
		return err
	}

	if err := shim.WriteBundle(dir, payload, man.Params(p.callable, p.role), man.Table()); err != nil {
		return fmt.Errorf("write bundle %s: %w", dir, err)
	}

	return nil
}

// rawToMap decodes a JSON object into a map. An empty payload, JSON null, or the
// empty string the Martian adapter writes for a stage with no outputs yields an
// empty map. Any other non-object payload (an array, a number, or corrupt JSON)
// is an error, so genuinely malformed outputs surface instead of silently
// becoming empty.
func rawToMap(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		return out, nil
	}

	if trimmed[0] != '{' {
		return nil, fmt.Errorf("decode payload: %w, got %.32s", errNotObject, trimmed)
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber() // keep 42.0 from collapsing to 42 across the bundle round-trip

	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	return out, nil
}
