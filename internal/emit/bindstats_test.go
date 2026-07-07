package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/ir"
)

// valueSimple reports whether a binding value is, at every leaf, a literal or a
// whole-field ref (no dotted projection) — the subset a driver-side map can
// resolve with a plain field pick, no projection engine (issue #221 Lever 1).
func valueSimple(v ir.Value) bool {
	switch {
	case v.Literal != nil:
		return true
	case v.Ref != nil:
		return !strings.Contains(v.Ref.Output, ".")
	case v.Array != nil:
		for _, e := range v.Array {
			if !valueSimple(e) {
				return false
			}
		}

		return true
	case v.Object != nil:
		for _, e := range v.Object {
			if !valueSimple(e) {
				return false
			}
		}

		return true
	}

	return true
}

func bindingsSimple(bs []ir.Binding) bool {
	for _, b := range bs {
		if !valueSimple(b.Value) {
			return false
		}
	}

	return true
}

func anyFileLeaf(params []ir.Param, structs map[string]*ir.StructType) bool {
	for _, p := range params {
		if hasFileLeaf(p, structs) {
			return true
		}
	}

	return false
}

func calleeIn(prog *ir.Program, callable string) ([]ir.Param, bool) {
	if s, ok := prog.Stages[callable]; ok {
		return s.In, true
	}

	if p, ok := prog.Pipelines[callable]; ok {
		return p.In, true
	}

	return nil, false
}

// TestBindStatsCellRanger measures the issue-#221 Lever-1 ceiling: of the
// boundary BIND tasks (sub-pipeline entry binds + pipeline return binds that the
// plan leaves as kindPlainBind), how many are the SIMPLE driver-resolvable case
// (all bindings literal/whole-field-ref AND no file leaf in the destination
// params) vs must stay a task (file-leaf → Lever 2, or dotted projection →
// deferred). Opt-in via CELLRANGER_HOME; a pure static IR/plan analysis (no run).
func TestBindStatsCellRanger(t *testing.T) {
	home := os.Getenv("CELLRANGER_HOME")
	if home == "" {
		t.Skip("CELLRANGER_HOME not set; skipping boundary-bind eligibility census")
	}

	ast, err := frontend.Parse(filepath.Join(home, "__tr_dry.mro"), []string{filepath.Join(home, "mro")}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	plan := buildPlan(prog, featureSet{})

	// L1 = no-file-leaf simple (Lever 1 eligible). fileRename = file-bearing but
	// every binding a whole-field ref (pure rename/select of already-staged
	// leaves — Lever 2 "reference-not-copy" eligible). projection = a dotted
	// projection or other reshape (needs the projection engine either way).
	var l1, fileRename, projection, total int
	classify := func(bindings []ir.Binding, dest []ir.Param) {
		total++
		file := anyFileLeaf(dest, prog.Structs)
		simple := bindingsSimple(bindings)
		switch {
		case !file && simple:
			l1++
		case file && simple:
			fileRename++
		default:
			projection++
		}
	}

	for name, p := range prog.Pipelines {
		pp := plan.pipes[name]
		for _, c := range p.Calls {
			if pp.calls[c.Name].kind != kindPlainBind {
				continue
			}

			if in, ok := calleeIn(prog, c.Callable); ok {
				classify(c.Bindings, in)
			}
		}

		if pp.retFwd == "" && len(p.Returns) > 0 {
			classify(p.Returns, p.Out)
		}
	}

	t.Logf("boundary binds: %d total", total)
	t.Logf("  LEVER 1 (no-file-leaf, driver map):            %d  (%.0f%%)", l1, 100*float64(l1)/float64(total))
	t.Logf("  LEVER 2 (file-bearing whole-field rename):     %d  (%.0f%%)", fileRename, 100*float64(fileRename)/float64(total))
	t.Logf("  projection (dotted projection, needs engine):     %d  (%.0f%%)", projection, 100*float64(projection)/float64(total))
	t.Logf("  => Lever 1 alone removes ~%.0f%% of boundary binds; Lever 1+2 removes ~%.0f%%",
		100*float64(l1)/float64(total), 100*float64(l1+fileRename)/float64(total))
}
