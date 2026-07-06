package emit

import (
	"errors"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
)

// TestStageHeadEscapesBakedPaths guards #183: a $ (or \) in a baked path — here
// the stage-code path — must be gstringLit-escaped so Groovy does not resolve it
// as a variable in the process-script GString (a MissingPropertyException at task
// launch, or silently wrong args), the way the -srcarg values already are.
func TestStageHeadEscapesBakedPaths(t *testing.T) {
	g := genCtx{
		entry:   "TOP",
		mroFile: "pipeline.mro",
		mre:     "mre",
		shell:   "/x/shell",
		code:    map[string]string{"S": "lib$v2/run.py"},
	}
	s := &ir.Stage{Name: "S", Lang: ir.LangPy}

	out := g.stageHead("main", s)

	if !strings.Contains(out, `lib\$v2/run.py`) {
		t.Errorf("stage-code path with a $ must be escaped; got:\n%s", out)
	}
}

// TestCheckBakedPathsRejectsQuote guards #183: a single quote in a baked path
// cannot survive the Groovy-GString + bash single-quoting, so it must be rejected
// loudly rather than corrupting the emitted command.
func TestCheckBakedPathsRejectsQuote(t *testing.T) {
	err := checkBakedPaths(genCtx{code: map[string]string{"S": "a'b.py"}})
	if !errors.Is(err, apperror.ErrUnsupported) {
		t.Errorf("a single quote in a stage src path must be rejected, got %v", err)
	}

	// A clean set of paths passes.
	if err := checkBakedPaths(genCtx{mre: "mre", shell: "/x/s", code: map[string]string{"S": "lib$v2/run.py"}}); err != nil {
		t.Errorf("clean paths must not be rejected, got %v", err)
	}
}
