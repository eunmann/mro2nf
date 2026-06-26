package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/shim"
)

const filePerm = 0o644

// readFile reads a JSON file, returning nil for an empty path.
func readFile(path string) (json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return data, nil
}

func readChunk(path string) (shim.ChunkDef, error) {
	chunk := shim.ChunkDef{Args: map[string]json.RawMessage{}}

	data, err := readFile(path)
	if err != nil {
		return chunk, err
	}

	// A non-split stage runs as a single chunk with no per-chunk args.
	if len(data) == 0 {
		return chunk, nil
	}

	if err := json.Unmarshal(data, &chunk); err != nil {
		return chunk, fmt.Errorf("parse chunk %s: %w", path, err)
	}

	return chunk, nil
}

// readChunkData reads the chunk defs array and the ordered, comma-separated
// per-chunk outs files.
func readChunkData(defsPath, outsList string) ([]shim.ChunkDef, []json.RawMessage, error) {
	defsRaw, err := readFile(defsPath)
	if err != nil {
		return nil, nil, err
	}

	var defs []shim.ChunkDef
	if err := json.Unmarshal(defsRaw, &defs); err != nil {
		return nil, nil, fmt.Errorf("parse chunk defs %s: %w", defsPath, err)
	}

	var outs []json.RawMessage

	for _, file := range splitComma(outsList) {
		data, err := readFile(file)
		if err != nil {
			return nil, nil, err
		}

		outs = append(outs, data)
	}

	return defs, outs, nil
}

func readFileList(paths []string) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(paths))

	for _, p := range paths {
		data, err := readFile(p)
		if err != nil {
			return nil, err
		}

		out = append(out, data)
	}

	return out, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}

	return writeRaw(path, data)
}

func writeRaw(path string, data []byte) error {
	if path == "" || path == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}

		return nil
	}

	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// writeChunkFiles writes each chunk to a zero-padded file so that a lexical
// sort of the filenames recovers chunk order downstream.
func writeChunkFiles(dir string, defs []shim.ChunkDef) error {
	for i, def := range defs {
		name := fmt.Sprintf("chunk_%05d.json", i)
		if err := writeJSON(filepath.Join(dir, name), def); err != nil {
			return err
		}
	}

	return nil
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))

	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}
