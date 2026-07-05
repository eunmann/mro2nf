package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// TestStageCmdNativeRunner pins the #79 stage-hop swap: with nativeRunner a
// Python stage phase execs the embedded run_stage.py directly — no mre, no
// -shell adapter — while keeping the -vmemgb/-monitor suffixes the call sites
// rely on; comp/exec stages and the default mode keep the adapter invocation.
func TestStageCmdNativeRunner(t *testing.T) {
	g := genCtx{
		entry: "P", mroFile: "pipeline.mro", mre: "/x/mre", shell: "/x/shell.py",
		runnerBase: "${projectDir}/_assets/runner",
		monitor:    true,
		features:   featureSet{nativeRunner: true},
		code:       map[string]string{"S": "/code/stage", "C": "/code/bin"},
	}
	pyStage := &ir.Stage{Name: "S", Lang: ir.LangPy}
	compStage := &ir.Stage{Name: "C", Lang: ir.LangComp}

	py := g.stageCmd("main", pyStage, "4")
	want := `'python3' '${projectDir}/_assets/runner/run_stage.py' main -stagecode '/code/stage' -call 'P' -mro 'pipeline.mro' -vmemgb 4 -monitor`

	if py != want {
		t.Errorf("native-runner py stageCmd:\n got %q\nwant %q", py, want)
	}

	// A container backend rebinds runnerBase to the baked in-image path.
	gc := g
	gc.runnerBase = ctrRunner

	if got := gc.stageCmd("main", pyStage, ""); !strings.Contains(got, "'"+ctrRunner+"/run_stage.py'") {
		t.Errorf("container native-runner must exec the baked runner path: %q", got)
	}

	if comp := g.stageCmd("main", compStage, ""); !strings.Contains(comp, "'/x/mre' main -shell '/x/shell.py'") {
		t.Errorf("comp stage must keep the adapter path: %q", comp)
	}

	gd := genCtx{
		entry: "P", mroFile: "pipeline.mro", mre: "/x/mre", shell: "/x/shell.py",
		code: map[string]string{"S": "/code/stage"},
	}
	if def := gd.stageCmd("main", pyStage, ""); !strings.Contains(def, "'/x/mre' main -shell '/x/shell.py'") {
		t.Errorf("default mode must keep the adapter path: %q", def)
	}
}

// TestStageCmdSrcArgs pins #113: src args declared after the stage code path
// (`src exec "code.py a b"`) must ride to the adapter as one -srcarg flag each,
// in declaration order — and a stage without src args must emit none.
func TestStageCmdSrcArgs(t *testing.T) {
	g := genCtx{
		entry: "P", mroFile: "pipeline.mro", mre: "/x/mre", shell: "/x/shell.py",
		code: map[string]string{"E": "/code/tool.py"},
	}

	got := g.stageCmd("main", &ir.Stage{Name: "E", Lang: ir.LangExec, SrcArgs: []string{"3", "hello"}}, "")
	want := `'/x/mre' main -shell '/x/shell.py' -stagecode '/code/tool.py' -lang exec -call 'P' -mro 'pipeline.mro' -srcarg '3' -srcarg 'hello'`

	if got != want {
		t.Errorf("exec stageCmd with src args:\n got %q\nwant %q", got, want)
	}

	if plain := g.stageCmd("main", &ir.Stage{Name: "E", Lang: ir.LangExec}, ""); strings.Contains(plain, "-srcarg") {
		t.Errorf("stage without src args must not emit -srcarg: %q", plain)
	}
}

// TestWriteRunner pins the embedded runner assets: both files land under
// _assets/runner/ and the shim is importable as `martian` (sibling of
// run_stage.py, which is python's sys.path[0]).
func TestWriteRunner(t *testing.T) {
	dir := t.TempDir()

	if err := writeRunner(dir); err != nil {
		t.Fatalf("writeRunner: %v", err)
	}

	for _, rel := range []string{"_assets/runner/run_stage.py", "_assets/runner/martian.py"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing embedded runner asset %s: %v", rel, err)
		}
	}
}
