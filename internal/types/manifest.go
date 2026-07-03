package types

import (
	"encoding/json"
	"fmt"
	"maps"
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

// Table returns a file-leaf walk Table over the manifest's struct definitions,
// augmented with each callable's output bundle as a struct type keyed by the
// callable name.
//
// A param may be typed as a callable's outputs rather than a declared struct —
// e.g. `matrix_computer_outs _SLFE_MATRIX_COMPUTER`, forwarding a sub-pipeline's
// whole outputs. Martian treats such a value as a struct whose fields are that
// callable's outputs, so the file-leaf walk must descend into it. Without this,
// the struct name resolves to no fields, the walk leaves the value untouched, and
// every file the bundle carries stays an unmarked, task-local path — which then
// vanishes on the next isolated worker (AWS Batch/S3, HealthOmics). Genuine
// struct definitions win on any name collision.
func (m Manifest) Table() *Table {
	merged := make(map[string]*ir.StructType, len(m.Callables)+len(m.Structs))

	for name, c := range m.Callables {
		if len(c.Out) == 0 {
			continue
		}

		merged[name] = &ir.StructType{Name: name, Fields: c.Out}
	}

	maps.Copy(merged, m.Structs)

	return NewTable(merged)
}

// Param roles select which of a callable's parameter sets a producer walks when
// building an output bundle.
const (
	RoleIn      = "in"      // a callee's input params (bind/forkbind)
	RoleOut     = "out"     // a callable's output params (join/merge/return/publish)
	RoleMainOut = "mainout" // a split stage's per-chunk main outputs (out + chunkOut)
	RoleChunkIn = "chunkin" // a split stage's per-chunk input params
)

// Params returns the parameter set for a callable under the given role. An
// unknown callable or role yields nil (no file leaves to rewrite).
func (m Manifest) Params(callable, role string) []ir.Param {
	c, ok := m.Callables[callable]
	if !ok {
		return nil
	}

	switch role {
	case RoleIn:
		return c.In
	case RoleOut:
		return c.Out
	case RoleChunkIn:
		return c.ChunkIn
	case RoleMainOut:
		return append(append([]ir.Param(nil), c.Out...), c.ChunkOut...)
	default:
		return nil
	}
}
