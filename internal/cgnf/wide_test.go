package cgnf

import (
	"maps"
	"path/filepath"
	"regexp"
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
// workflow, as WIDE.TREE's 150 do, is wired in runs of them; one whose call
// would take more than maxTakes channels, as WIDE.FANIN's of 210 stages
// would, is wired in its caller. Each workflow mixes the outs bundles its
// own units publish for OUTS, so neither the 210 nor the 1600 nodes WIDE's
// rs reads outgrow the entry workflow. No workflow weighs more than
// maxWeight or takes more than maxTakes channels, and each defines every
// channel before it reads it. A node's own wiring is a few lines and one
// per edge of it, the same in any pipeline.
func TestEmitBoundsEveryWorkflow(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name   string
		p      wide.Pipeline
		labels []string
	}{
		{"calls", wide.New(32, 150, 210), []string{"part of WIDE.TREE", "WIDE.B31", "WIDE.TREE", "part of WIDE.FANIN"}},
		{"outs", wide.New(2, 3, 1600), []string{"part of WIDE", "WIDE.B1", "part of WIDE.FANIN"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if c.p.Stages < 500 {
				t.Fatalf("the wide pipeline has %d stage calls, want at least 500", c.p.Stages)
			}

			checkBounds(t, c.p, c.labels)
		})
	}
}

// checkBounds checks every workflow of p's project against the JVM's
// limits, and that the workflows labelled exist.
func checkBounds(t *testing.T, p wide.Pipeline, labels []string) {
	t.Helper()

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

	labelled := func(label string) bool {
		return slices.ContainsFunc(bodies, func(b workflowBody) bool { return strings.Contains(tree[b.file], "\n// "+label+"\nworkflow ") })
	}

	for _, want := range labels {
		if !labelled(want) {
			t.Errorf("no workflow wires %s", want)
		}
	}

	if labelled("WIDE.FANIN") {
		t.Errorf("WIDE.FANIN, whose call takes over %d channels, has a workflow of its own", maxTakes)
	}

	checkWiring(t, bodies)
	checkPublished(t, bodies, len(p.Rs)+1)
}

// checkWiring checks that each workflow defines every channel of the
// project before it reads it, from a take, a statement or a call's emit,
// emits only channels it defines, and that each call of a sub-workflow
// passes its takes.
func checkWiring(t *testing.T, bodies []workflowBody) {
	t.Helper()

	takes, channels := map[string][]string{}, map[string]bool{}

	for _, b := range bodies {
		takes[b.name] = b.takes

		for _, c := range b.takes {
			channels[c] = true
		}

		for _, l := range b.main {
			if lhs, _ := assignment(l); lhs != "" {
				channels[lhs] = true
			}
		}
	}

	for _, b := range bodies {
		checkBody(t, b, takes, channels)
	}
}

// checkBody checks b's wiring against the takes of each workflow and the
// project's channels.
func checkBody(t *testing.T, b workflowBody, takes map[string][]string, channels map[string]bool) {
	t.Helper()

	defined := map[string]bool{}
	for _, c := range b.takes {
		defined[c] = true
	}

	for _, l := range b.main {
		lhs, rhs := assignment(l)

		for _, c := range readNames(rhs) {
			if channels[c] && !defined[c] {
				t.Errorf("%s: workflow %s reads %s before it defines it: %s", b.file, b.name, c, l)
			}
		}

		if m := subCall.FindStringSubmatch(rhs); m != nil && m[2] != strings.Join(takes[m[1]], ", ") {
			t.Errorf("%s: workflow %s calls %s(%s), which takes %v", b.file, b.name, m[1], m[2], takes[m[1]])
		}

		defined[lhs] = true
	}

	for _, e := range b.emits {
		if !defined[e] {
			t.Errorf("%s: workflow %s emits %s, which it does not define", b.file, b.name, e)
		}
	}
}

var (
	identifier = regexp.MustCompile(`^\w+$`)
	subCall    = regexp.MustCompile(`^(W\d+_\w+)\((.*)\)$`)
	quoted     = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	nameRead   = regexp.MustCompile(`(?:^|[^.\w])(\w+)`)
)

// assignment is the channel a statement defines, if any, and the
// expression it evaluates.
func assignment(line string) (string, string) {
	lhs, rhs, ok := strings.Cut(line, " = ")
	if !ok || !identifier.MatchString(lhs) {
		return "", line
	}

	return lhs, rhs
}

// readNames are the names an expression reads, less quoted strings and
// members.
func readNames(expr string) []string {
	ms := nameRead.FindAllStringSubmatch(quoted.ReplaceAllString(expr, "''"), -1)

	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m[1]
	}

	return out
}

var (
	publishedNode = regexp.MustCompile(`PUBLISHED\(\w+, (\d+), `)
	publishedSub  = regexp.MustCompile(`(W\d+_\w+)\.out\.published`)
)

// checkPublished checks that the n nodes the pipeline outs read each
// publish once, in the workflow that wires it, that each workflow mixes
// its sources at most maxTakes at a time and emits the mix, which its
// caller mixes once, and that OUTS reads the entry workflow's mix.
func checkPublished(t *testing.T, bodies []workflowBody, n int) {
	t.Helper()

	nodes, mixed, emitted := map[string]int{}, map[string]int{}, map[string]bool{}

	for _, b := range bodies {
		emitted[b.name] = slices.Contains(b.emits, "published")

		for _, l := range b.main {
			if strings.HasPrefix(l, "published = channel.empty().mix(") {
				checkMix(t, b, l, emitted[b.name])
				countMatches(nodes, publishedNode, l)
				countMatches(mixed, publishedSub, l)
			}
		}
	}

	if len(nodes) != n || slices.ContainsFunc(slices.Collect(maps.Values(nodes)), func(k int) bool { return k != 1 }) {
		t.Errorf("%d nodes publish, some more than once (%v), want %d once each", len(nodes), nodes, n)
	}

	for name, e := range emitted {
		if e != (mixed[name] == 1) {
			t.Errorf("workflow %s emits published %t, and its caller mixes it %d times", name, e, mixed[name])
		}
	}

	entry := slices.IndexFunc(bodies, func(b workflowBody) bool { return b.name == "{" })
	if entry < 0 || !slices.Contains(bodies[entry].main, "outs_in = published.toList().map { xs -> ordered(xs) }") {
		t.Errorf("OUTS does not read the entry workflow's published mix")
	}
}

// checkMix checks b's published statement l: a sub-workflow emits it, and
// it mixes at most maxTakes sources at a time.
func checkMix(t *testing.T, b workflowBody, l string, emitted bool) {
	t.Helper()

	if b.name != "{" && !emitted {
		t.Errorf("%s: workflow %s mixes what it publishes but does not emit it", b.file, b.name)
	}

	for _, m := range strings.Split(l, ".mix(")[1:] {
		if k := len(publishedNode.FindAllString(m, -1)) + len(publishedSub.FindAllString(m, -1)); k > maxTakes {
			t.Errorf("%s: workflow %s mixes %d sources at once (max %d)", b.file, b.name, k, maxTakes)
		}
	}
}

// countMatches counts in counts each of re's first submatches in l.
func countMatches(counts map[string]int, re *regexp.Regexp, l string) {
	for _, m := range re.FindAllStringSubmatch(l, -1) {
		counts[m[1]]++
	}
}

// TestEmitBoundsEachNode: a node's wiring is a constant few lines and one
// per root, zip, preflight, gate and read of it, and one more per phase of
// a split stage.
func TestEmitBoundsEachNode(t *testing.T) {
	dir := t.TempDir()
	if err := wide.New(2, 3, 0).Write(dir); err != nil {
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
