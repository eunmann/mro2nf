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
		monitor:  true,
		features: featureSet{nativeRunner: true},
	}

	py := g.stageCmd("main", "/code/stage", ir.LangPy, "4")
	want := `'python3' '${projectDir}/_assets/runner/run_stage.py' main -stagecode '/code/stage' -call 'P' -mro 'pipeline.mro' -vmemgb 4 -monitor`

	if py != want {
		t.Errorf("native-runner py stageCmd:\n got %q\nwant %q", py, want)
	}

	if comp := g.stageCmd("main", "/code/bin", ir.LangComp, ""); !strings.Contains(comp, "'/x/mre' main -shell '/x/shell.py'") {
		t.Errorf("comp stage must keep the adapter path: %q", comp)
	}

	gd := genCtx{entry: "P", mroFile: "pipeline.mro", mre: "/x/mre", shell: "/x/shell.py"}
	if def := gd.stageCmd("main", "/code/stage", ir.LangPy, ""); !strings.Contains(def, "'/x/mre' main -shell '/x/shell.py'") {
		t.Errorf("default mode must keep the adapter path: %q", def)
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
