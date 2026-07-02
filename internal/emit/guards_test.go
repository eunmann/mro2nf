package emit_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestParseTargetInvalid checks an unknown -target fails as ErrUnsupported.
func TestParseTargetInvalid(t *testing.T) {
	if _, err := emit.ParseTarget("bogus"); !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("ParseTarget(bogus): want ErrUnsupported, got %v", err)
	}

	if tgt, err := emit.ParseTarget(""); err != nil || tgt != emit.TargetLocal {
		t.Errorf("ParseTarget(\"\") = (%v, %v), want (local, nil)", tgt, err)
	}
}

// TestEmitNoEntry checks a program without a top-level call is rejected before
// any output is written.
func TestEmitNoEntry(t *testing.T) {
	dir := t.TempDir()

	err := emit.Emit(&ir.Program{}, emit.Options{OutDir: dir})
	if err == nil || !strings.Contains(err.Error(), "no entry call") {
		t.Fatalf("Emit without entry: want 'no entry call' error, got %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "main.nf")); statErr == nil {
		t.Error("Emit wrote main.nf despite rejecting the program")
	}
}

// TestEmitContainerMissingSources checks the container-target fail-loud
// contract: a missing -mre (and a nonexistent path) fail the transpile instead
// of surfacing later as a cryptic docker-build COPY error.
func TestEmitContainerMissingSources(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	opts := emit.Options{
		OutDir: t.TempDir(), Target: emit.TargetAWSBatch, MROFile: "pipeline.mro",
		StageCode: map[string]string{"SUM_SQUARES": t.TempDir(), "REPORT": t.TempDir()},
	}

	if err := emit.Emit(prog, opts); err == nil || !strings.Contains(err.Error(), "-mre") {
		t.Errorf("container target without -mre: want error naming -mre, got %v", err)
	}

	opts.Mre = filepath.Join(t.TempDir(), "does-not-exist")
	if err := emit.Emit(prog, opts); err == nil || !strings.Contains(err.Error(), opts.Mre) {
		t.Errorf("container target with missing mre file: want error naming the path, got %v", err)
	}
}

// TestWarningsPreflightCallRef checks a preflight bound to another call's
// output produces the runs-in-DAG-order warning (the arm no fixture triggers).
func TestWarningsPreflightCallRef(t *testing.T) {
	prog := &ir.Program{
		Stages: map[string]*ir.Stage{"S": {Name: "S", Out: []ir.Param{{Name: "y", BaseType: "int"}}}},
		Pipelines: map[string]*ir.Pipeline{
			"T": {Name: "T", Calls: []ir.Call{
				{Name: "A", Callable: "S"},
				{Name: "CHECK", Callable: "S", Preflight: true, Bindings: []ir.Binding{
					{Param: "x", Value: ir.Value{Ref: &ir.Ref{Kind: "call", ID: "A", Output: "y"}}},
				}},
			}},
		},
	}

	warnings := emit.Warnings(prog)
	found := false

	for _, w := range warnings {
		if strings.Contains(w, "T.CHECK") && strings.Contains(w, "preflight") {
			found = true
		}
	}

	if !found {
		t.Errorf("want a preflight-in-DAG-order warning for T.CHECK, got %v", warnings)
	}
}

// TestEmitHealthOmicsTemplateEntries checks every entry input is declared in
// parameter-template.json (undeclared parameters are rejected by HealthOmics,
// which would silently disable the override path), with the S3 wording for
// file-bearing inputs.
func TestEmitHealthOmicsTemplateEntries(t *testing.T) {
	mre := filepath.Join(t.TempDir(), "mre")
	if err := os.WriteFile(mre, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := "../../testdata/entry_file"

	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	code := map[string]string{}
	for name := range prog.Stages {
		code[name] = t.TempDir()
	}

	dir := t.TempDir()
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: mre, MROFile: "pipeline.mro", Target: emit.TargetHealthOmics,
		StageCode: code,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "parameter-template.json"))
	if err != nil {
		t.Fatal(err)
	}

	var tmpl map[string]struct {
		Description string `json:"description"`
		Optional    bool   `json:"optional"`
	}

	if err := json.Unmarshal(data, &tmpl); err != nil {
		t.Fatalf("parse template: %v", err)
	}

	reads, ok := tmpl["reads"]
	if !ok || !reads.Optional || !strings.Contains(reads.Description, "S3 URI") {
		t.Errorf("file input 'reads' entry = %+v (present=%v), want optional with S3 wording", reads, ok)
	}

	if _, ok := tmpl["container"]; !ok {
		t.Error("template missing the required 'container' parameter")
	}
}

// TestBindSpecSplitFlag checks a map call's split binding round-trips into its
// written bindspec with "split": true — the flag forkbind forks on.
func TestBindSpecSplitFlag(t *testing.T) {
	dir := emitFixture(t, "fork_min", map[string]string{"SCALE": "/x/scale"})

	specs, err := filepath.Glob(filepath.Join(dir, "_assets", "bindspecs", "BIND_*SCALE*.json"))
	if err != nil || len(specs) == 0 {
		t.Fatalf("no SCALE bindspec found: %v", err)
	}

	data, err := os.ReadFile(specs[0])
	if err != nil {
		t.Fatal(err)
	}

	var spec map[string]struct {
		Split bool `json:"split"`
	}

	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	split := false
	for _, e := range spec {
		split = split || e.Split
	}

	if !split {
		t.Errorf("map-call bindspec has no split:true entry:\n%s", data)
	}
}
