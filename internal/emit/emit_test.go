package emit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/eunmann/martian-nextflow/internal/emit"
	"github.com/eunmann/martian-nextflow/internal/frontend"
)

func loadAndEmit(t *testing.T) string {
	t.Helper()

	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	opts := emit.Options{
		OutDir:  dir,
		Mre:     "mre",
		Shell:   "/x/martian_shell.py",
		MROFile: "pipeline.mro",
		StageCode: map[string]string{
			"SUM_SQUARES": "/x/sum_squares",
			"REPORT":      "/x/report",
		},
	}

	if err := emit.Emit(prog, opts); err != nil {
		t.Fatalf("emit: %v", err)
	}

	return dir
}

func TestEmitFiles(t *testing.T) {
	dir := loadAndEmit(t)

	for _, rel := range []string{
		"main.nf",
		"nextflow.config",
		"entry_args/data.json",
		"types.json",
		"modules/pipe_SUM_SQUARE_PIPELINE.nf",
		"modules/stage_SUM_SQUARES.nf",
		"modules/stage_REPORT.nf",
		"bindspecs/BIND_19_SUM_SQUARE_PIPELINE__SUM_SQUARES.json",
		"bindspecs/BIND_19_SUM_SQUARE_PIPELINE__REPORT.json",
		"bindspecs/BIND_19_SUM_SQUARE_PIPELINE__return.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected file %s: %v", rel, err)
		}
	}
}

func TestEmitEntryArgs(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "entry_args", "data.json"))
	if err != nil {
		t.Fatalf("read entry args: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse entry args: %v", err)
	}

	want := map[string]any{"values": []any{float64(1), float64(2), float64(3)}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entry args mismatch (-want +got):\n%s", diff)
	}
}

func TestEmitModules(t *testing.T) {
	dir := loadAndEmit(t)

	checks := map[string][]string{
		"main.nf": {
			"include { SUM_SQUARE_PIPELINE } from './modules/pipe_SUM_SQUARE_PIPELINE.nf'",
			"SUM_SQUARE_PIPELINE(pipeargs)",
		},
		"modules/pipe_SUM_SQUARE_PIPELINE.nf": {
			"workflow SUM_SQUARE_PIPELINE {",
			"include { wf_SUM_SQUARES as wf_19_SUM_SQUARE_PIPELINE__SUM_SQUARES }",
			// Call outputs are value channels so they can feed multiple consumers.
			".out).first()",
		},
		"modules/stage_SUM_SQUARES.nf": {
			"process SUM_SQUARES_SPLIT {",
			"process SUM_SQUARES_MAIN {",
			"process SUM_SQUARES_JOIN {",
			"workflow wf_SUM_SQUARES {",
			// Paths are single-quoted so spaces/metacharacters are safe.
			"-stagecode '/x/sum_squares'",
			// Per-chunk resources reach the scheduler via dynamic directives
			// reading the chunk's resolved resources carried as a val.
			"cpus { (res?.threads",
			"memory { (res?.mem_gb",
		},
		"modules/stage_REPORT.nf": {"process REPORT {"},
	}

	for rel, wants := range checks {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)

			continue
		}

		for _, want := range wants {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s missing %q", rel, want)
			}
		}
	}
}
