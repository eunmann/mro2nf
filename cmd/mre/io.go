package main

import (
	"encoding/json"
	"errors"
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

// readJoinRes decodes the split's joinres.json (the join phase's resource
// override); an empty path is no override.
func readJoinRes(path string) (shim.Resources, error) {
	var res shim.Resources

	raw, err := readFile(path)
	if err != nil || raw == nil {
		return res, err
	}

	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("parse join resources %s: %w", path, err)
	}

	return res, nil
}

// chunkDefPrefix and chunkOutPrefix name the per-chunk bundles a JOIN gathers:
// the split writes chunk_%05d defs and the emitted MAIN names each output
// out_${chunk.baseName}.
const (
	chunkDefPrefix = "chunk_"
	chunkOutPrefix = "out_" + chunkDefPrefix
)

// readChunkData gathers the per-chunk DEFINITION and OUTPUT bundles staged in
// chunkDir, in chunk-index order, checking both number exactly the chunks the
// split's chunks.json lists. Both are resolved through ReadBundle, so a
// file-typed leaf the split phase created and returned in a chunk's args (e.g.
// a custom-filetype chunk arg) is rewritten from the split worker's ephemeral
// scratch to a locally-staged path the join worker can open — an object-store
// join never sees the split's absent scratch path (#217). chunks.json supplies
// only the count, for the same reason. Zero chunks is a 0-chunk split (the
// join still runs, with no chunks).
func readChunkData(chunksFile, chunkDir string) ([]shim.ChunkDef, []json.RawMessage, error) {
	count, err := readChunkCount(chunksFile)
	if err != nil {
		return nil, nil, err
	}

	defDirs, err := gatherIndexed(chunkDir, chunkDefPrefix, count)
	if err != nil {
		return nil, nil, fmt.Errorf("gather chunk defs: %w", err)
	}

	outDirs, err := gatherIndexed(chunkDir, chunkOutPrefix, count)
	if err != nil {
		return nil, nil, fmt.Errorf("gather chunk outs: %w", err)
	}

	defRaws, err := readBundleList(defDirs)
	if err != nil {
		return nil, nil, err
	}

	defs := make([]shim.ChunkDef, 0, len(defRaws))
	for _, raw := range defRaws {
		def, err := decodeChunk(raw)
		if err != nil {
			return nil, nil, err
		}

		defs = append(defs, def)
	}

	outs, err := readBundleList(outDirs)
	if err != nil {
		return nil, nil, err
	}

	return defs, outs, nil
}

// errNoChunksFile reports a join without the chunks.json it counts against.
var errNoChunksFile = errors.New("join requires -chunksfile <the split's chunks.json>")

// readChunkCount returns the number of chunk defs the split wrote to
// chunks.json (a JSON array; null for a split that returned no chunks).
func readChunkCount(path string) (int, error) {
	if path == "" {
		return 0, errNoChunksFile
	}

	data, err := readFile(path)
	if err != nil {
		return 0, err
	}

	var defs []json.RawMessage
	if err := json.Unmarshal(data, &defs); err != nil {
		return 0, fmt.Errorf("parse chunk defs %s: %w", path, err)
	}

	return len(defs), nil
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
