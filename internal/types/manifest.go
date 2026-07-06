package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/eunmann/mro2nf/internal/ir"
)

// filePerm is the mode for generated project artifacts (not secrets).
const filePerm = 0o644

// Manifest is the serialized type information the runtime shim loads (via
// `-types`) so it can run the file-leaf walk without the IR. It is written once
// by the transpiler and staged into the project.
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

// Write serializes the manifest to path.
func (m Manifest) Write(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	// Skip a byte-identical rewrite so a re-transpile preserves types.json's mtime:
	// it is staged as a `path` input into every stage, and Nextflow's default
	// -resume cache keys on path+size+mtime, so an unchanged rewrite would bust
	// every task's cache on a re-emit (#188). A real change still rewrites.
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}

	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	return nil
}

// LoadManifest reads a manifest written by Write.
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

// Param roles select which of a callable's parameter sets a producer walks when
// building an output bundle.
const (
	RoleIn      = "in"      // a callee's input params (bind/forkbind)
	RoleOut     = "out"     // a callable's output params (join/merge/return/publish)
	RoleMainOut = "mainout" // a split stage's per-chunk main outputs (out + chunkOut)
	RoleChunkIn = "chunkin" // a split stage's per-chunk input params
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
