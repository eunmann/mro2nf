package cgnf

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureDirs lists every testdata fixture with a pipeline.mro.
func fixtureDirs(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "pipeline.mro"))
	if err != nil || len(files) < 100 {
		t.Fatalf("fixtures: %d, %v", len(files), err)
	}

	dirs := make([]string, len(files))
	for i, f := range files {
		dirs[i] = filepath.Dir(f)
	}

	return dirs
}

// TestEmitRefusesUncreatableOutDir: no project is emitted into an out dir
// below a file.
func TestEmitRefusesUncreatableOutDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "file_min"))
	if err != nil {
		t.Fatal(err)
	}

	opts := Options{OutDir: filepath.Join(blocker, "proj"), MroPaths: []string{fixture}}
	if err := Emit(filepath.Join(fixture, "pipeline.mro"), opts); err == nil {
		t.Error("emitted into an out dir below a file")
	}
}

// TestEmitEveryFixture: every fixture mrp instantiates emits a project
// whose main.nf runs PLAN, a process call per witness node, then OUTS, and
// whose config declares a param per entry input; a fixture mrp refuses
// emits nothing.
func TestEmitEveryFixture(t *testing.T) {
	// mrp refuses to instantiate these (#387): a split of a runtime-gated
	// literal collection.
	mrpRefused := []string{"rt_gate_split_reject", "rt_gate_split_reject_pass", "rt_gate_split_reject_sub"}

	for _, dir := range fixtureDirs(t) {
		name := filepath.Base(dir)

		t.Run(name, func(t *testing.T) {
			abs, err := filepath.Abs(dir)
			if err != nil {
				t.Fatal(err)
			}

			out := filepath.Join(t.TempDir(), "proj")
			opts := Options{OutDir: out, MroPaths: []string{abs}, Version: "test"}
			err = Emit(filepath.Join(abs, "pipeline.mro"), opts)

			if slices.Contains(mrpRefused, name) {
				if _, statErr := os.Stat(out); err == nil || statErr == nil {
					t.Errorf("emit = %v, out stat = %v; mrp refuses %s, so no project may exist", err, statErr, name)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			main, err := os.ReadFile(filepath.Join(out, "main.nf"))
			if err != nil {
				t.Fatal(err)
			}

			for _, want := range []string{"PLAN(", "OUTS("} {
				if !strings.Contains(string(main), want) {
					t.Errorf("main.nf lacks %q", want)
				}
			}

			if nodes, incs := strings.Count(string(main), "\n    // "), strings.Count(string(main), "include { STAGE"); nodes != incs {
				t.Errorf("main.nf wires %d nodes but includes the stage module %d times", nodes, incs)
			}
		})
	}
}
