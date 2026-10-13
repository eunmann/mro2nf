package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// writeTestBundle writes payload as a bundle directory with no file leaves.
func writeTestBundle(t *testing.T, dir string, payload map[string]any) {
	t.Helper()

	if err := shim.WriteBundle(dir, payload, nil, types.NewTable(nil), shim.Opts{}); err != nil {
		t.Fatalf("write bundle %s: %v", dir, err)
	}
}

// writeTestFile writes content to path and returns path.
func writeTestFile(t *testing.T, path, content string) string {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
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
		{name: "deleted args", args: []string{"args"}, want: errUnknownCommand},
		{name: "deleted run", args: []string{"run"}, want: errUnknownCommand},
		{name: "deleted post", args: []string{"post"}, want: errUnknownCommand},
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
