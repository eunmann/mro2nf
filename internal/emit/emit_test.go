package emit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		"entry_args.json",
		"bindspecs/BIND_SUM_SQUARE_PIPELINE__SUM_SQUARES.json",
		"bindspecs/BIND_SUM_SQUARE_PIPELINE__REPORT.json",
		"bindspecs/BIND_SUM_SQUARE_PIPELINE__return.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected file %s: %v", rel, err)
		}
	}
}

func TestEmitEntryArgs(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "entry_args.json"))
	if err != nil {
		t.Fatalf("read entry args: %v", err)
	}

	if got := strings.TrimSpace(string(data)); got != `{"values":[1,2,3]}` {
		t.Errorf("entry args = %s, want {\"values\":[1,2,3]}", got)
	}
}

func TestEmitMainNF(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "main.nf"))
	if err != nil {
		t.Fatalf("read main.nf: %v", err)
	}

	nf := string(data)
	for _, want := range []string{
		"workflow SUM_SQUARE_PIPELINE {",
		"process SUM_SQUARES_SPLIT {",
		"process SUM_SQUARES_MAIN {",
		"process SUM_SQUARES_JOIN {",
		"process REPORT {",
		"workflow wf_SUM_SQUARES {",
		"-stagecode /x/sum_squares",
	} {
		if !strings.Contains(nf, want) {
			t.Errorf("main.nf missing %q", want)
		}
	}
}
