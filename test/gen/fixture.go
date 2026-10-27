package gen

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed stub.py
var stubCode []byte

// stubSpec is the stage description the stub reads from spec.json beside it:
// parameter types by name, every struct type a value may hold, and the seed
// its draws start from, none (0) for a generated program's stage.
type stubSpec struct {
	Name      string                 `json:"name"`
	In        [][2]string            `json:"in"`
	Out       [][2]string            `json:"out"`
	Split     bool                   `json:"split"`
	ChunkIn   [][2]string            `json:"chunk_in"`
	ChunkOut  [][2]string            `json:"chunk_out"`
	Structs   map[string][][2]string `json:"structs"`
	FileTypes []string               `json:"filetypes"`
	Fail      *Fail                  `json:"fail"`
	Seed      uint64                 `json:"seed,omitempty"`
}

// Fixture files are only read back by the user who runs the differential.
const (
	dirPerm  = 0o750
	filePerm = 0o600
)

// WriteFixture writes p as a fixture directory in the testdata layout:
// pipeline.mro, and stages/<name>/ holding each stage's stub and spec.
func (p *Program) WriteFixture(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create fixture dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(p.MRO()), filePerm); err != nil {
		return fmt.Errorf("write pipeline.mro: %w", err)
	}

	structs := p.structSpecs()

	for _, s := range p.Stages {
		if err := p.writeStage(dir, s, structs); err != nil {
			return fmt.Errorf("stage %s: %w", s.Name, err)
		}
	}

	return nil
}

func (p *Program) writeStage(dir string, s *Stage, structs map[string][][2]string) error {
	return writeStub(filepath.Join(dir, filepath.FromSlash(StageDir(s.Name))), stubSpec{
		Name:      s.Name,
		In:        paramSpecs(s.In),
		Out:       paramSpecs(s.Out),
		Split:     s.Split,
		ChunkIn:   paramSpecs(s.ChunkIn),
		ChunkOut:  paramSpecs(s.ChunkOut),
		Structs:   structs,
		FileTypes: append([]string{}, p.FileTypes...),
		Fail:      s.Fail,
	})
}

// writeStub writes the stub and its spec into stageDir.
func writeStub(stageDir string, s stubSpec) error {
	if err := os.MkdirAll(stageDir, dirPerm); err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}

	var spec bytes.Buffer

	enc := json.NewEncoder(&spec)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("encode spec: %w", err)
	}

	if err := os.WriteFile(filepath.Join(stageDir, "spec.json"), spec.Bytes(), filePerm); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}

	if err := os.WriteFile(filepath.Join(stageDir, "__init__.py"), stubCode, filePerm); err != nil {
		return fmt.Errorf("write stub: %w", err)
	}

	return nil
}

// structSpecs maps every struct type name a value may carry to its fields:
// the user structs, and each callable, whose outputs are its struct.
func (p *Program) structSpecs() map[string][][2]string {
	out := map[string][][2]string{}

	for _, s := range p.Structs {
		out[s.Name] = paramSpecs(s.Fields)
	}

	for _, s := range p.Stages {
		out[s.Name] = paramSpecs(s.Out)
	}

	for _, pl := range p.Pipelines {
		out[pl.Name] = paramSpecs(pl.Out)
	}

	return out
}

func paramSpecs(ps []Param) [][2]string {
	out := make([][2]string, len(ps))
	for i, q := range ps {
		out[i] = [2]string{q.Name, q.Type.String()}
	}

	return out
}
