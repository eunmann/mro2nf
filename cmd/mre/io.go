package main

import (
	"encoding/json"
	"fmt"
	"os"
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
	var chunk shim.ChunkDef

	data, err := readFile(path)
	if err != nil {
		return chunk, err
	}

	if err := json.Unmarshal(data, &chunk); err != nil {
		return chunk, fmt.Errorf("parse chunk %s: %w", path, err)
	}

	return chunk, nil
}

func readChunkData(defsPath, outsPath string) ([]shim.ChunkDef, []json.RawMessage, error) {
	defsRaw, err := readFile(defsPath)
	if err != nil {
		return nil, nil, err
	}

	var defs []shim.ChunkDef
	if err := json.Unmarshal(defsRaw, &defs); err != nil {
		return nil, nil, fmt.Errorf("parse chunk defs %s: %w", defsPath, err)
	}

	outsRaw, err := readFile(outsPath)
	if err != nil {
		return nil, nil, err
	}

	var outs []json.RawMessage
	if err := json.Unmarshal(outsRaw, &outs); err != nil {
		return nil, nil, fmt.Errorf("parse chunk outs %s: %w", outsPath, err)
	}

	return defs, outs, nil
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
