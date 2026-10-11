package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2Run runs mro2nf -emitter=v2 on fixture into a fresh dir and returns the
// project dir and the error.
func v2Run(t *testing.T, fixture string, flags ...string) (string, error) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "proj")
	dir := filepath.Join("..", "..", "testdata", fixture)
	args := append([]string{"-emitter=v2", "-o", out, "-mre", "/opt/bin/mre", "-shell", "/opt/adapters/python/martian_shell.py", "-mropath", dir}, flags...)

	return out, run(append(args, filepath.Join(dir, "pipeline.mro")))
}

// TestEmitterV2: -emitter=v2 emits the call-graph project, its mrjob the
// one built beside mre and its adapters the shell's, and the flag is left
// out of the usage text.
func TestEmitterV2(t *testing.T) {
	out, err := v2Run(t, "file_min")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := os.ReadFile(filepath.Join(out, "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"params.mro2nf_mrjob = '/opt/bin/mrjob'", "params.mro2nf_adapters = '/opt/adapters'", "params.x = null"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	if _, err := os.Stat(filepath.Join(out, "stage.nf")); err != nil {
		t.Errorf("no stage module: %v", err)
	}

	fs := flag.NewFlagSet("mro2nf", flag.ContinueOnError)

	var usage bytes.Buffer

	fs.SetOutput(&usage)
	defineTranspileFlags(fs)
	fs.Usage()

	if strings.Contains(usage.String(), "emitter") || !strings.Contains(usage.String(), "-mropath") {
		t.Errorf("usage shows -emitter or lost the other flags:\n%s", usage.String())
	}
}

// TestEmitterV2Refuses: the call-graph emitter refuses an unknown emitter,
// the old emitter's modes and a missing adapter, and a pipeline mrp refuses
// before any stage runs (#387), emitting no project.
func TestEmitterV2Refuses(t *testing.T) {
	mro := filepath.Join("..", "..", "testdata", "file_min", "pipeline.mro")

	if err := run([]string{"-emitter=v3", "-o", t.TempDir(), mro}); !errors.Is(err, errBadEmitter) {
		t.Errorf("-emitter=v3: %v, want errBadEmitter", err)
	}

	if _, err := v2Run(t, "file_min", "-native"); !errors.Is(err, errV2Flag) {
		t.Errorf("-native: %v, want errV2Flag", err)
	}

	if err := run([]string{"-emitter=v2", "-o", t.TempDir(), mro}); !errors.Is(err, errV2NoShell) {
		t.Errorf("no -shell: %v, want errV2NoShell", err)
	}

	out, err := v2Run(t, "rt_gate_split_reject")
	if err == nil || !strings.Contains(err.Error(), "RTG.B input value:") {
		t.Errorf("rt_gate_split_reject: %v, want mrp's refusal", err)
	}

	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused transpile left %s (stat: %v)", out, err)
	}
}
