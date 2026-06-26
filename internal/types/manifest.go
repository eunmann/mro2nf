package types

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/eunmann/martian-nextflow/internal/ir"
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

// Table returns a file-leaf walk Table over the manifest's struct definitions.
func (m Manifest) Table() *Table {
	return NewTable(m.Structs)
}
