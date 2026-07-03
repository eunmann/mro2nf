package emit

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
)

// TestStageCmdCompSrcArgs checks that a comp stage's bare binary name and its
// `martian <stage>` src args are both baked into the mre invocation, so the shim
// reconstructs `mrjob cr_lib martian <stage> <phase> ...`. This is the form every
// CellRanger Rust stage uses (`src comp "cr_lib martian <stage>"`); dropping the
// src args (the prior behavior) left the binary with no registry selector.
func TestStageCmdCompSrcArgs(t *testing.T) {
	g := genCtx{
		mre: "/opt/mro2nf/mre", shell: "/opt/cr/martian_shell.py", mrjob: "/opt/cr/mrjob",
		entry: "SC_RNA_COUNTER_CS", mroFile: "cr-entry.mro",
		code:    map[string]string{"ALIGN_AND_COUNT": "cr_lib"},
		srcArgs: map[string][]string{"ALIGN_AND_COUNT": {"martian", "align_and_count"}},
	}
	s := &ir.Stage{Name: "ALIGN_AND_COUNT", Lang: ir.LangComp}

	cmd := g.stageCmd("main", s, "")

	for _, want := range []string{
		"-stagecode 'cr_lib'",
		"-srcargs 'martian align_and_count'",
		"-mrjob '/opt/cr/mrjob'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("stageCmd = %q, missing %q", cmd, want)
		}
	}
}

// TestStageCmdNoSrcArgs checks a py stage (no src args) emits no -srcargs flag.
func TestStageCmdNoSrcArgs(t *testing.T) {
	g := genCtx{
		mre: "mre", shell: "/s/martian_shell.py", entry: "P", mroFile: "p.mro",
		code: map[string]string{"S": "/abs/stages/s"},
	}
	s := &ir.Stage{Name: "S", Lang: ir.LangPy}

	cmd := g.stageCmd("main", s, "")

	if strings.Contains(cmd, "-srcargs") {
		t.Errorf("py stage stageCmd must not emit -srcargs, got %q", cmd)
	}
	if !strings.Contains(cmd, "-stagecode '/abs/stages/s'") {
		t.Errorf("stageCmd = %q, missing py stagecode", cmd)
	}
}
