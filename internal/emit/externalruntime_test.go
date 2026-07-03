package emit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
)

// TestEmitExternalRuntime checks that with ExternalRuntime the container target
// vendors nothing: no Dockerfile, no runtime/ build context, and the passed
// mre/shell/stagecode paths are baked verbatim (not rewritten to /opt/mro2nf/*).
// This is the CellRanger case — the image already provides the runtime.
func TestEmitExternalRuntime(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	ss, _ := filepath.Abs("../../testdata/split_test/stages/sum_squares")
	shell := "/opt/cellranger/external/martian/adapters/python/martian_shell.py"
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: "/opt/mro2nf/mre", Mrjob: "/opt/cellranger/external/martian/bin/mrjob",
		Shell: shell, MROFile: "pipeline.mro", Target: emit.TargetAWSBatch,
		Container: "ecr/img:latest", ExternalRuntime: true,
		StageCode: map[string]string{"SUM_SQUARES": ss, "REPORT": ss},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); !os.IsNotExist(err) {
		t.Error("ExternalRuntime must not write a Dockerfile")
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime")); !os.IsNotExist(err) {
		t.Error("ExternalRuntime must not create a runtime/ build context")
	}

	mod, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(mod)
	if strings.Contains(got, "/opt/mro2nf/stages") {
		t.Errorf("ExternalRuntime must not rewrite stagecode to /opt/mro2nf/stages:\n%s", got)
	}
	if !strings.Contains(got, "-stagecode '"+ss+"'") {
		t.Errorf("module must bake the passed stagecode %q verbatim", ss)
	}
	if !strings.Contains(got, "-shell '"+shell+"'") {
		t.Errorf("module must bake the passed shell path verbatim")
	}
}
