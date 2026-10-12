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
// one built beside mre and its adapters the shell's, confined under
// -confine-outputs, and the flag is left out of the usage text, which
// keeps every other flag's default.
func TestEmitterV2(t *testing.T) {
	out, err := v2Run(t, "file_min")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := os.ReadFile(filepath.Join(out, "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"params.mro2nf_mrjob = '/opt/bin/mrjob'", "params.mro2nf_adapters = '/opt/adapters'", "params.mro2nf_confine = false", "params.x = null"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	confined, err := v2Run(t, "file_min", "-confine-outputs")
	if err != nil {
		t.Fatal(err)
	}

	if cfg, err := os.ReadFile(filepath.Join(confined, "nextflow.config")); err != nil || !strings.Contains(string(cfg), "params.mro2nf_confine = true") {
		t.Errorf("-confine-outputs does not confine the stage tasks (%v):\n%s", err, cfg)
	}

	if _, err := os.Stat(filepath.Join(out, "stage.nf")); err != nil {
		t.Errorf("no stage module: %v", err)
	}

	fs := flag.NewFlagSet("mro2nf", flag.ContinueOnError)

	var usage bytes.Buffer

	fs.SetOutput(&usage)
	defineTranspileFlags(fs)

	if err := fs.Parse([]string{"-mre", "/x/mre"}); err != nil {
		t.Fatal(err)
	}

	fs.Usage()

	if strings.Contains(usage.String(), "emitter") || !strings.Contains(usage.String(), "-mropath") {
		t.Errorf("usage shows -emitter or lost the other flags:\n%s", usage.String())
	}

	for _, want := range []string{`(default "mre")`, `(default "local")`, "(default true)"} {
		if !strings.Contains(usage.String(), want) {
			t.Errorf("usage lacks the default %s:\n%s", want, usage.String())
		}
	}
}

// TestEmitterV2ResolvesToolsAsTheOldEmitter: v2 takes its adapters from
// -shell made absolute against the cwd, and from a .mro2nf.yml shell against
// the config file's dir, as the transpile options resolve them (#266).
func TestEmitterV2ResolvesToolsAsTheOldEmitter(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "file_min"))
	if err != nil {
		t.Fatal(err)
	}

	cwd, cfgDir := t.TempDir(), t.TempDir()
	cfg := filepath.Join(cfgDir, ".mro2nf.yml")

	if err := os.WriteFile(cfg, []byte("shell: tools/adapters/python/martian_shell.py\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(cwd)

	for name, tc := range map[string]struct {
		flags []string
		want  string
	}{
		"flag":   {[]string{"-shell", "adapters/python/martian_shell.py"}, filepath.Join(cwd, "adapters")},
		"config": {[]string{"-config", cfg}, filepath.Join(cfgDir, "tools", "adapters")},
	} {
		out := filepath.Join(t.TempDir(), "proj")
		args := append([]string{"-emitter=v2", "-o", out, "-mre", "/opt/bin/mre", "-mropath", dir}, tc.flags...)

		if err := run(append(args, filepath.Join(dir, "pipeline.mro"))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		got, err := os.ReadFile(filepath.Join(out, "nextflow.config"))
		if err != nil {
			t.Fatal(err)
		}

		if want := "params.mro2nf_adapters = '" + tc.want + "'"; !strings.Contains(string(got), want) {
			t.Errorf("%s: nextflow.config lacks %q:\n%s", name, want, got)
		}
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

	for _, flag := range []string{"-native", "-allow-external-files", "-inline-pipelines=false"} {
		if _, err := v2Run(t, "file_min", flag); !errors.Is(err, errV2Flag) {
			t.Errorf("%s: %v, want errV2Flag", flag, err)
		}
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
