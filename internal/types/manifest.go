package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// Manifest is the serialized type information the runtime shim loads (via
// `-types`) so it can run the file-leaf walk without the IR. The transpiler
// writes into each task's script the Subset of the callables its commands
// name (Encoded.Subset).
type Manifest struct {
	// Structs holds every struct definition, keyed by name.
	Structs map[string]*ir.StructType `json:"structs"`
	// Callables holds each stage's and pipeline's parameter sets, keyed by name.
	Callables map[string]Callable `json:"callables"`
}

// Callable carries the typed parameter sets the shim needs to rewrite paths for
// a single stage or pipeline. ChunkIn/ChunkOut are empty for non-split stages
// and pipelines.
type Callable struct {
	In       []ir.Param `json:"in"`
	Out      []ir.Param `json:"out"`
	ChunkIn  []ir.Param `json:"chunkIn,omitempty"`
	ChunkOut []ir.Param `json:"chunkOut,omitempty"`
}

// BuildManifest collects type information for every callable in the program.
func BuildManifest(prog *ir.Program) Manifest {
	m := Manifest{
		Structs:   prog.Structs,
		Callables: make(map[string]Callable, len(prog.Stages)+len(prog.Pipelines)),
	}

	for name, s := range prog.Stages {
		m.Callables[name] = Callable{In: s.In, Out: s.Out, ChunkIn: s.ChunkIn, ChunkOut: s.ChunkOut}
	}

	for name, p := range prog.Pipelines {
		m.Callables[name] = Callable{In: p.In, Out: p.Out}
	}

	return m
}

// Subset returns the manifest of the named callables alone: their parameter
// sets, and the structs those reach directly or through other structs. An
// unknown name is left out, so the shim fails loudly on it.
func (m Manifest) Subset(callables ...string) Manifest {
	sub := Manifest{Structs: map[string]*ir.StructType{}, Callables: map[string]Callable{}}

	var reach func(params []ir.Param)
	reach = func(params []ir.Param) {
		for _, p := range params {
			st, ok := m.Structs[p.BaseType]
			if !ok || sub.Structs[p.BaseType] != nil {
				continue
			}

			sub.Structs[p.BaseType] = st
			reach(st.Fields)
		}
	}

	for _, name := range callables {
		c, ok := m.Callables[name]
		if !ok {
			continue
		}

		sub.Callables[name] = c

		for _, params := range [][]ir.Param{c.In, c.Out, c.ChunkIn, c.ChunkOut} {
			reach(params)
		}
	}

	return sub
}

// Encoded is a manifest with each struct and callable encoded once, so the
// encoding of any subset of it is assembled without an encoding error.
type Encoded struct {
	m Manifest
	// structs and callables map each name to its encoded `"name":value`
	// object member.
	structs, callables map[string]string
}

// Encode encodes each of the manifest's structs and callables.
func (m Manifest) Encode() (Encoded, error) {
	e := Encoded{
		m:         m,
		structs:   make(map[string]string, len(m.Structs)),
		callables: make(map[string]string, len(m.Callables)),
	}

	for name, st := range m.Structs {
		member, err := encodeMember(name, st)
		if err != nil {
			return Encoded{}, fmt.Errorf("encode struct %s: %w", name, err)
		}

		e.structs[name] = member
	}

	for name, c := range m.Callables {
		member, err := encodeMember(name, c)
		if err != nil {
			return Encoded{}, fmt.Errorf("encode callable %s: %w", name, err)
		}

		e.callables[name] = member
	}

	return e, nil
}

// encodeMember encodes name and v as a JSON object member.
func encodeMember(name string, v any) (string, error) {
	key, err := json.Marshal(name)
	if err != nil {
		return "", fmt.Errorf("encode name: %w", err)
	}

	value, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode value: %w", err)
	}

	return string(key) + ":" + string(value), nil
}

// Subset returns the compact JSON of the manifest's Subset of callables, in
// the shape LoadManifest reads, with every object's members sorted by name.
func (e Encoded) Subset(callables ...string) string {
	sub := e.m.Subset(callables...)

	var b strings.Builder

	b.WriteString(`{"structs":{`)
	writeMembers(&b, slices.Sorted(maps.Keys(sub.Structs)), e.structs)
	b.WriteString(`},"callables":{`)
	writeMembers(&b, slices.Sorted(maps.Keys(sub.Callables)), e.callables)
	b.WriteString("}}")

	return b.String()
}

// writeMembers writes the encoded members of names, comma-separated.
func writeMembers(b *strings.Builder, names []string, encoded map[string]string) {
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(encoded[name])
	}
}

// LoadManifest reads a manifest rendered by Encoded.Subset.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest

	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read manifest: %w", err)
	}

	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse manifest: %w", err)
	}

	return m, nil
}

// Table returns a file-leaf walk Table over the manifest's struct definitions.
func (m Manifest) Table() *Table {
	return NewTable(m.Structs)
}

// Param roles select which of a callable's parameter sets a bundle's values
// are walked with.
const (
	RoleIn      = "in"      // a callable's input params (mre args, plan)
	RoleOut     = "out"     // a callable's output params (mre run, post, outs, publish-layout)
	RoleMainOut = "mainout" // a split stage's per-chunk main outputs, out + chunkOut (mre run)
	RoleChunkIn = "chunkin" // a split stage's per-chunk input params (mre run)
)

var (
	// ErrUnknownCallable reports a callable name missing from a configured
	// manifest.
	ErrUnknownCallable = errors.New("callable not in type manifest")
	// ErrUnknownRole reports a role outside the Role* constants.
	ErrUnknownRole = errors.New("unknown param role")
)

// Params returns the parameter set for a callable under the given role. The
// unconfigured zero manifest (no Callables at all — the no `-types` path,
// where there is nothing to rewrite) yields nil for any callable; a configured
// manifest fails loudly on an unknown callable or role, since a silently-nil
// parameter set would skip path rewrites and surface only as dangling files
// downstream.
func (m Manifest) Params(callable, role string) ([]ir.Param, error) {
	if m.Callables == nil {
		return nil, nil
	}

	c, ok := m.Callables[callable]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCallable, callable)
	}

	switch role {
	case RoleIn:
		return c.In, nil
	case RoleOut:
		return c.Out, nil
	case RoleChunkIn:
		return c.ChunkIn, nil
	case RoleMainOut:
		return append(append([]ir.Param(nil), c.Out...), c.ChunkOut...), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
}
