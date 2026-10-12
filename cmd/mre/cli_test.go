package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// writeTestBundle writes payload as a bundle directory with no file leaves.
func writeTestBundle(t *testing.T, dir string, payload map[string]any) {
	t.Helper()

	if err := shim.WriteBundle(dir, payload, nil, types.NewTable(nil), nil); err != nil {
		t.Fatalf("write bundle %s: %v", dir, err)
	}
}

// readTestBundle loads a bundle's payload into a generic map.
func readTestBundle(t *testing.T, dir string) map[string]any {
	t.Helper()

	raw, err := shim.ReadBundle(dir)
	if err != nil {
		t.Fatalf("read bundle %s: %v", dir, err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode bundle %s: %v", dir, err)
	}

	return out
}

// writeTestFile writes content to path and returns path.
func writeTestFile(t *testing.T, path, content string) string {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// writeTestManifest writes man to path as the generated scripts render a
// task's manifest (Encoded.Subset over every callable) and returns path.
func writeTestManifest(t *testing.T, path string, man types.Manifest) string {
	t.Helper()

	enc, err := man.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}

	return writeTestFile(t, path, enc.Subset(slices.Collect(maps.Keys(man.Callables))...))
}

// readJSONFile decodes the JSON file at path into v.
func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// TestExitCode pins the exit-code mapping: an ASSERT-class stage failure (even
// wrapped) uses the distinct non-retryable code, a job failure mrp retries the
// one retryable code; anything else exits 1.
func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "wrapped job assert", err: fmt.Errorf("main: %w", job.ErrAssert), want: job.AssertExitCode},
		{name: "wrapped transient job failure", err: fmt.Errorf("main: %w", job.ErrTransient), want: job.TransientExitCode},
		{name: "ordinary error", err: errUsage, want: 1},
		{name: "wrapped ordinary error", err: fmt.Errorf("read: %w", os.ErrNotExist), want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestRunDispatch covers the top-level subcommand dispatch: no args is a usage
// error, an unknown or deleted subcommand wraps errUnknownCommand, and version
// succeeds.
func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want error
	}{
		{name: "empty args", args: nil, want: errUsage},
		{name: "unknown subcommand", args: []string{"frobnicate"}, want: errUnknownCommand},
		{name: "deleted bind", args: []string{"bind"}, want: errUnknownCommand},
		{name: "deleted split", args: []string{"split"}, want: errUnknownCommand},
		{name: "version", args: []string{"version"}, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(t.Context(), tc.args); !errors.Is(err, tc.want) {
				t.Errorf("run(%v) = %v, want %v", tc.args, err, tc.want)
			}
		})
	}
}

// TestRawToMap covers the empty-payload tolerances (empty bytes, null, the empty
// string the adapter writes for a stage with no outputs), the not-an-object
// errors, and whole-number float fidelity via json.Number.
func TestRawToMap(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[string]any
		wantErr error
	}{
		{name: "empty object", raw: `{}`, want: map[string]any{}},
		{name: "padded null", raw: ` null `, want: map[string]any{}},
		{name: "empty json string", raw: `""`, want: map[string]any{}},
		{name: "empty bytes", raw: ``, want: map[string]any{}},
		{name: "array is not an object", raw: `[1]`, wantErr: errNotObject},
		{name: "number is not an object", raw: `5`, wantErr: errNotObject},
		{name: "whole-number float fidelity", raw: `{"x": 42.0}`, want: map[string]any{"x": json.Number("42.0")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rawToMap(json.RawMessage(tc.raw))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("rawToMap(%q) error = %v, want %v", tc.raw, err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("rawToMap(%q): %v", tc.raw, err)
			}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("rawToMap(%q) mismatch (-want +got):\n%s", tc.raw, diff)
			}
		})
	}
}

// TestProducerManifestNoTypes checks that a producer with no -types path loads
// an empty manifest (no file leaves to rewrite) instead of failing.
func TestProducerManifestNoTypes(t *testing.T) {
	prod := &producer{}

	man, err := prod.manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}

	if len(man.Structs) != 0 || len(man.Callables) != 0 {
		t.Errorf("empty producer manifest = %+v, want empty", man)
	}

	params, err := man.Params("ANY", types.RoleIn)
	if err != nil {
		t.Fatalf("Params on empty manifest: %v", err)
	}

	if params != nil {
		t.Errorf("empty manifest Params = %v, want nil", params)
	}
}

// TestRunPublishLayoutSmoke drives publish-layout end to end from a raw
// marker-bearing sidecar: pipeline_outs.json carries the rewritten value
// tree, manifest.json.gz gunzips to the versioned output index, and no
// layout.json is written, as nothing reads one (#293).
func TestRunPublishLayoutSmoke(t *testing.T) {
	dir := t.TempDir()
	man := types.Manifest{Callables: map[string]types.Callable{
		"PIPE": {Out: []ir.Param{{Name: "aln", BaseType: "bam", IsFile: true}}},
	}}
	manPath := writeTestManifest(t, filepath.Join(dir, "types.json"), man)

	sidecar := writeTestFile(t, filepath.Join(dir, "data.json"),
		`{"aln":"`+marker("L0000")+`"}`)
	outDir := filepath.Join(dir, "outs")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir outs: %v", err)
	}

	argv := []string{"publish-layout", "-types", manPath, "-callable", "PIPE", "-sidecar", sidecar, "-dir", outDir}
	if err := run(t.Context(), argv); err != nil {
		t.Fatalf("run publish-layout: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "layout.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("publish-layout wrote layout.json (stat: %v)", err)
	}

	var outs map[string]any

	readJSONFile(t, filepath.Join(outDir, "pipeline_outs.json"), &outs)

	if diff := cmp.Diff(map[string]any{"aln": "aln.bam"}, outs); diff != "" {
		t.Errorf("pipeline_outs.json mismatch (-want +got):\n%s", diff)
	}

	gz, err := os.Open(filepath.Join(outDir, "manifest.json.gz"))
	if err != nil {
		t.Fatalf("open manifest.json.gz: %v", err)
	}
	defer func() { _ = gz.Close() }()

	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatalf("gunzip manifest: %v", err)
	}

	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var index struct {
		SchemaVersion int             `json:"schema_version"`
		Pipeline      string          `json:"pipeline"`
		Outputs       []manifestEntry `json:"outputs"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	if index.SchemaVersion != 1 || index.Pipeline != "PIPE" {
		t.Errorf("manifest header = %d/%q, want 1/PIPE", index.SchemaVersion, index.Pipeline)
	}

	wantOutputs := []manifestEntry{{Path: "aln.bam", BaseType: "bam", IsDir: false}}
	if diff := cmp.Diff(wantOutputs, index.Outputs); diff != "" {
		t.Errorf("manifest outputs mismatch (-want +got):\n%s", diff)
	}
}
