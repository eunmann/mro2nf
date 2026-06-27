package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/eunmann/mro2nf/internal/shim"
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

// decodeChunk decodes a chunk bundle's resolved payload into a ChunkDef. Empty
// input is a non-split stage's single chunk (no per-chunk args).
func decodeChunk(raw json.RawMessage) (shim.ChunkDef, error) {
	chunk := shim.ChunkDef{Args: map[string]json.RawMessage{}}

	if len(raw) == 0 {
		return chunk, nil
	}

	if err := json.Unmarshal(raw, &chunk); err != nil {
		return chunk, fmt.Errorf("parse chunk: %w", err)
	}

	if chunk.Args == nil {
		chunk.Args = map[string]json.RawMessage{}
	}

	return chunk, nil
}

// readChunkData reads the chunk defs array (a plain summary file) and the
// ordered, comma-separated per-chunk output bundle directories.
func readChunkData(defsPath, outsList string) ([]shim.ChunkDef, []json.RawMessage, error) {
	defsRaw, err := readFile(defsPath)
	if err != nil {
		return nil, nil, err
	}

	var defs []shim.ChunkDef
	if err := json.Unmarshal(defsRaw, &defs); err != nil {
		return nil, nil, fmt.Errorf("parse chunk defs %s: %w", defsPath, err)
	}

	outs, err := readBundleList(splitComma(outsList))
	if err != nil {
		return nil, nil, err
	}

	return defs, outs, nil
}

// readBundle resolves a bundle directory into its payload (file leaves rewritten
// to absolute paths), wrapping the error for the caller's context.
func readBundle(dir string) (json.RawMessage, error) {
	raw, err := shim.ReadBundle(dir)
	if err != nil {
		return nil, fmt.Errorf("read input bundle: %w", err)
	}

	return raw, nil
}

// readBundleList resolves each bundle directory in dirs into its payload.
func readBundleList(dirs []string) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(dirs))

	for _, d := range dirs {
		data, err := readBundle(d)
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
