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

// TestEmitConfig checks nextflow.config declares every executor profile and the
// auto-retry analog, so a -profile run resolves and transient failures retry.
func TestEmitConfig(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "nextflow.config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	cfg := string(data)
	for _, want := range []string{
		"params.outdir = 'results'",
		"standard { process.executor = 'local' }",
		"slurm    { process.executor = 'slurm' }",
		"sge      { process.executor = 'sge' }",
		"lsf      { process.executor = 'lsf' }",
		"pbs      { process.executor = 'pbs' }",
		"process.executor = 'awsbatch'",
		"k8s { process.executor = 'k8s' }",
		"task.exitStatus == 42 ? 'terminate' : 'retry'",
		"maxRetries = 2",
		"params.aws_queue = null",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config missing %q", want)
		}
	}
}

// TestEmitMonitorAndContainer checks the -monitor and -container options reach
// the generated stage commands and config.
func TestEmitMonitorAndContainer(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	err = emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: "mre", Shell: "/x/martian_shell.py", MROFile: "pipeline.mro",
		Monitor: true, Container: "ecr/mre:latest",
		StageCode: map[string]string{"SUM_SQUARES": "/x/sum_squares", "REPORT": "/x/report"},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	mod, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(mod), " -monitor") {
		t.Error("stage command missing -monitor with Monitor: true")
	}

	cfg, err := os.ReadFile(filepath.Join(dir, "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(cfg), "container = 'ecr/mre:latest'") {
		t.Error("nextflow.config missing process.container line")
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
			// Bind outputs are value channels, so callee results feed multiple
			// consumers directly (no redundant, warning-triggering .first()).
			"ch_SUM_SQUARES = wf_19_SUM_SQUARE_PIPELINE__SUM_SQUARES(BIND_19_SUM_SQUARE_PIPELINE__SUM_SQUARES.out)",
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
			// Static using(mem_gb=2) maps to the split/join phase memory.
			"memory '2 GB'",
		},
		"modules/stage_REPORT.nf": {
			"process REPORT {",
			// using(threads=0.5) rounds up to one whole CPU.
			"cpus 1",
		},
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
