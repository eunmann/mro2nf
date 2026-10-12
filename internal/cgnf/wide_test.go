package cgnf

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/test/wide"
)

// workflowBody is one workflow of a generated file: its takes, the
// statements of its main section and its emits.
type workflowBody struct {
	file, name         string
	takes, main, emits []string
}

// weight is the body's weight as the layout bounds it.
func (w workflowBody) weight() int {
	return linesWeight(w.main) + portCost*(len(w.takes)+len(w.emits))
}

// parseWorkflows lists the workflows of a generated .nf file.
func parseWorkflows(file, text string) []workflowBody {
	var (
		out     []workflowBody
		cur     *workflowBody
		section *[]string
	)

	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "workflow "):
			cur = &workflowBody{file: file, name: strings.TrimSuffix(strings.TrimPrefix(line, "workflow "), " {")}
			section = &cur.main
		case cur == nil:
		case line == "}":
			out = append(out, *cur)
			cur = nil
		case line == "    take:":
			section = &cur.takes
		case line == "    main:":
			section = &cur.main
		case line == "    emit:":
			section = &cur.emits
		case strings.TrimSpace(line) != "":
			*section = append(*section, strings.TrimSpace(line))
		}
	}

	return out
}

func emitWide(t *testing.T, p wide.Pipeline) (string, map[string]string) {
	t.Helper()

	dir := t.TempDir()
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	opts := Options{OutDir: out, MroPaths: []string{dir}, Version: "test"}

	if err := Emit(filepath.Join(dir, "pipeline.mro"), opts); err != nil {
		t.Fatal(err)
	}

	return dir, readTree(t, out)
}

// TestEmitBoundsEveryWorkflow (#503): a pipeline of over 500 stage calls of
// every shape compiles into workflows none of which outgrows the JVM's
// limits, whatever the pipeline's size: each Martian pipeline is a workflow
// of its own, the root's the entry workflow, and one whose calls outweigh a
// workflow, as WIDE.TREE's 150 do, is wired in runs of them; no workflow
// weighs more than maxWeight or takes more than maxTakes channels. A
// node's own wiring is a few lines and one per edge of it, the same in any
// pipeline.
func TestEmitBoundsEveryWorkflow(t *testing.T) {
	p := wide.New(32, 150)
	if p.Stages < 500 {
		t.Fatalf("the wide pipeline has %d stage calls, want at least 500", p.Stages)
	}

	_, tree := emitWide(t, p)

	var bodies []workflowBody

	for _, name := range slices.Sorted(maps.Keys(tree)) {
		if name == "main.nf" || strings.HasPrefix(name, subworkflowDir+"/") {
			bodies = append(bodies, parseWorkflows(name, tree[name])...)
		}
	}

	total := 0

	for _, b := range bodies {
		total += b.weight()

		if b.weight() > maxWeight || len(b.takes) > maxTakes {
			t.Errorf("%s: workflow %s weighs %d (max %d) and takes %d channels (max %d)", b.file, b.name, b.weight(), maxWeight, len(b.takes), maxTakes)
		}
	}

	if total < 4*maxWeight {
		t.Errorf("the wide pipeline weighs %d, too little to need more than a few workflows", total)
	}

	for _, want := range []string{"part of WIDE.TREE", "WIDE.B31", "WIDE.TREE"} {
		if !slices.ContainsFunc(bodies, func(b workflowBody) bool { return strings.Contains(tree[b.file], "\n// "+want+"\nworkflow ") }) {
			t.Errorf("no workflow wires %s", want)
		}
	}
}

// TestEmitBoundsEachNode: a node's wiring is a constant few lines and one
// per root, zip, preflight, gate and read of it, and one more per phase of
// a split stage.
func TestEmitBoundsEachNode(t *testing.T) {
	dir := t.TempDir()
	if err := wide.New(2, 3).Write(dir); err != nil {
		t.Fatal(err)
	}

	src, err := cg.ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	p, err := newProject(src, Options{MroPaths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}

	for i, n := range p.topo.Nodes {
		w, err := p.node(i, n)
		if err != nil {
			t.Fatal(err)
		}

		edges := len(n.Roots) + len(n.Zips) + len(n.Preflights) + len(n.Gates) + len(n.Reads)
		if n.Split {
			edges += 2
		}

		if len(w.lines) > 7+edges {
			t.Errorf("%s: %d lines for %d edges:\n%s", n.Fqid, len(w.lines), edges, strings.Join(w.lines, "\n"))
		}
	}
}
