package cgnf

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/test/wide"
	"github.com/google/go-cmp/cmp"
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

// TestEmitBoundsEveryWorkflow (#503, #541): a pipeline of over 500 stage
// calls of every shape compiles into workflows none of which outgrows the
// JVM's limits, whatever the pipeline's size or the shape of its .mro
// pipelines: the entry workflow calls runs of the nodes in dependency
// order, each a workflow of its own, filled until the next node would
// outgrow it, however the .mro nests them, so WIDE.TREE's 150 calls,
// WIDE.FANIN's 210 inputs and WIDE's 32 small BLOCK calls cost no workflow
// each. Each workflow mixes the outs bundles its own units publish for
// OUTS, so neither the 210 nor the 1600 nodes WIDE's rs reads outgrow the
// entry workflow. No workflow weighs more than maxWeight or takes more than
// maxTakes channels, and each defines every channel before it reads it. A
// node's own wiring is a few lines and one per edge of it, the same in any
// pipeline.
func TestEmitBoundsEveryWorkflow(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		p    wide.Pipeline
	}{
		{"calls", wide.New(32, 150, 210)},
		{"outs", wide.New(2, 3, 1600)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if c.p.Stages < 500 {
				t.Fatalf("the wide pipeline has %d stage calls, want at least 500", c.p.Stages)
			}

			checkBounds(t, c.p)
		})
	}
}

// checkBounds checks every workflow of p's project against the JVM's
// limits, and that the entry workflow calls every other, no two in turn of
// which would fit one.
func checkBounds(t *testing.T, p wide.Pipeline) {
	t.Helper()

	_, tree := emitWide(t, p)
	bodies := workflowBodies(tree)

	if total := checkLimits(t, bodies); total < 4*maxWeight {
		t.Errorf("the wide pipeline weighs %d, too little to need more than a few workflows", total)
	}

	checkFlat(t, bodies)
	checkWiring(t, bodies)
	checkPublished(t, bodies, len(p.Rs)+1)
}

// checkFlat checks that only the entry workflow calls sub-workflows, in
// the order they are numbered, and that no two it calls in turn weigh as
// little as half of maxWeight and take as few as half of maxTakes channels:
// each run of nodes ends only where the next node would outgrow it.
func checkFlat(t *testing.T, bodies []workflowBody) {
	t.Helper()

	var called []string

	for _, b := range bodies {
		for _, l := range b.main {
			m := subCall.FindStringSubmatch(l)
			if m == nil {
				continue
			}

			if b.name != "{" {
				t.Errorf("%s: workflow %s calls %s", b.file, b.name, m[1])
			}

			called = append(called, m[1])
		}
	}

	if len(called) < 2 {
		t.Fatalf("the entry workflow calls %d sub-workflows, want several", len(called))
	}

	body := func(name string) workflowBody {
		return bodies[slices.IndexFunc(bodies, func(b workflowBody) bool { return b.name == name })]
	}

	for j, name := range called {
		if want := "W" + strconv.Itoa(j); name != want {
			t.Errorf("sub-workflow call %d is %s, want %s", j, name, want)
		}

		if j == 0 {
			continue
		}

		a, b := body(called[j-1]), body(name)
		if a.weight()+b.weight() <= maxWeight/2 && len(a.takes)+len(b.takes) <= maxTakes/2 {
			t.Errorf("workflows %s and %s weigh %d and %d and take %d and %d channels, so they would fit one", a.name, b.name, a.weight(), b.weight(), len(a.takes), len(b.takes))
		}
	}
}

// workflowBodies lists the workflows of a project's main.nf and its
// sub-workflow modules.
func workflowBodies(tree map[string]string) []workflowBody {
	var bodies []workflowBody

	for _, name := range slices.Sorted(maps.Keys(tree)) {
		if name == "main.nf" || strings.HasPrefix(name, subworkflowDir+"/") {
			bodies = append(bodies, parseWorkflows(name, tree[name])...)
		}
	}

	return bodies
}

// checkLimits checks that no workflow weighs more than maxWeight or takes
// more than maxTakes channels, and returns their total weight.
func checkLimits(t *testing.T, bodies []workflowBody) int {
	t.Helper()

	total := 0

	for _, b := range bodies {
		total += b.weight()

		if b.weight() > maxWeight || len(b.takes) > maxTakes {
			t.Errorf("%s: workflow %s weighs %d (max %d) and takes %d channels (max %d)", b.file, b.name, b.weight(), maxWeight, len(b.takes), maxTakes)
		}
	}

	return total
}

// TestEmitBoundsPlan (#509): PLAN writes the forks of every entry-fed root
// as one record, so neither PLAN's outputs nor the entry workflow's wiring
// of them grows with the roots: a pipeline of over 500 entry-fed roots has
// the PLAN process and the PLAN wiring of one of a few, and each of its
// workflows stays within the JVM's limits.
func TestEmitBoundsPlan(t *testing.T) {
	t.Parallel()

	few, many := wide.Roots(1, 1, 1), wide.Roots(8, 600, 3)

	fewDir, small := emitWide(t, few)
	dir, large := emitWide(t, many)

	if got := entryFedRoots(t, dir); got != many.Roots || got < 500 {
		t.Fatalf("the pipeline has %d entry-fed roots, want %d, at least 500", got, many.Roots)
	}

	if d := cmp.Diff(planOf(small["main.nf"], fewDir), planOf(large["main.nf"], dir)); d != "" {
		t.Errorf("PLAN and its wiring grow with the entry-fed roots (-%d roots +%d roots):\n%s", few.Roots, many.Roots, d)
	}

	bodies := workflowBodies(large)
	checkLimits(t, bodies)
	checkWiring(t, bodies)
}

// entryFedRoots counts the fork roots the entry values feed in the
// pipeline.mro in dir.
func entryFedRoots(t *testing.T, dir string) int {
	t.Helper()

	src, err := cg.ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	p, err := newProject(src, Options{MroPaths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}

	n := 0

	for _, r := range p.topo.Roots {
		if r.Producer == "" {
			n++
		}
	}

	return n
}

// planOf is main.nf's PLAN process and the statements of its entry
// workflow that read PLAN's records, with the pipeline's dir, which they
// name, left out.
func planOf(main, dir string) []string {
	main = strings.ReplaceAll(main, dir, "")
	start := strings.Index(main, "process PLAN {")
	end := start + strings.Index(main[start:], "\n}\n")
	out := strings.Split(main[start:end], "\n")

	for _, b := range parseWorkflows("main.nf", main) {
		for _, l := range b.main {
			if strings.Contains(l, "PLAN.out.") {
				out = append(out, l)
			}
		}
	}

	return out
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
	subCall    = regexp.MustCompile(`^(W\d+)\((.*)\)$`)
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
	publishedSub  = regexp.MustCompile(`\b(W\d+)\.out\.` + publishedChannel)
)

// checkPublished checks that the n nodes the pipeline outs read each
// publish once, in the workflow that wires it, that each workflow mixes
// its sources at most maxTakes at a time and emits the mix, which its
// caller mixes once, and that OUTS reads the entry workflow's mix.
func checkPublished(t *testing.T, bodies []workflowBody, n int) {
	t.Helper()

	nodes, mixed, emitted := map[string]int{}, map[string]int{}, map[string]bool{}

	for _, b := range bodies {
		emitted[b.name] = slices.Contains(b.emits, publishedChannel)

		for _, l := range b.main {
			if strings.HasPrefix(l, publishedChannel+" = channel.empty().mix(") {
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
	if entry < 0 || !slices.Contains(bodies[entry].main, "outs_in = "+publishedChannel+".toList().map { xs -> ordered(xs) }") {
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
// per root, zip, gate and read of it, and one more per phase of a split
// stage; waiting for its preflights costs it none.
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

		edges := len(n.Roots) + len(n.Zips) + len(n.Gates) + len(n.Reads)
		if n.Split {
			edges += 2
		}

		if len(w.lines) > 7+edges {
			t.Errorf("%s: %d lines for %d edges:\n%s", n.Fqid, len(w.lines), edges, strings.Join(w.lines, "\n"))
		}
	}
}

// TestEmitWaitsOncePerPreflight (#512): Nextflow adds each edge of its DAG
// in time linear in the edges before it, so the project holds PLAN's
// records once per preflight, not each node's instances: one AFTER
// whatever the number of nodes below WIDE's one preflight, every one of
// which but the preflight gates on the held records.
func TestEmitWaitsOncePerPreflight(t *testing.T) {
	t.Parallel()

	after := regexp.MustCompile(`(?m)^\s*(\w+) = AFTER\(nodes, d\d+\)$`)
	gate := regexp.MustCompile(`GATE\((?:i\d+|channel\.of\(\[\[\], \[\], \[\], \[\]\]\)), (\w+), \[node: '`)

	for _, p := range []wide.Pipeline{wide.New(2, 3, 0), wide.New(32, 150, 0)} {
		_, tree := emitWide(t, p)
		nf := wiringNF(tree)

		held := after.FindAllStringSubmatch(nf, -1)
		if len(held) != 1 || strings.Count(nf, "AFTER(") != 1 {
			t.Fatalf("%d stage calls: %d AFTER calls, want 1", p.Stages, strings.Count(nf, "AFTER("))
		}

		gates := map[string]int{}
		for l := range strings.SplitSeq(nf, "\n") {
			countMatches(gates, gate, l)
		}

		if want := map[string]int{"nodes": 1, held[0][1]: p.Stages - 1}; !cmp.Equal(gates, want) {
			t.Errorf("%d stage calls: the records the nodes gate on are %v, want %v", p.Stages, gates, want)
		}
	}
}

// TestEmitReadsSharedOnce (#512): the meta every task shares reaches the
// nodes paired with PLAN's node records, which each node's GATE combines
// anyway, so no node combines it with its instances, an edge of
// Nextflow's DAG per node: of the whole project's workflows only the
// entry workflow's NODES and OUTS read it.
func TestEmitReadsSharedOnce(t *testing.T) {
	t.Parallel()

	_, tree := emitWide(t, wide.New(32, 150, 0))

	var got []string

	for _, b := range workflowBodies(tree) {
		if slices.Contains(b.takes, "shared") {
			t.Errorf("%s: workflow %s takes shared", b.file, b.name)
		}

		for _, l := range b.main {
			if _, rhs := assignment(l); slices.Contains(readNames(rhs), "shared") {
				head, _, _ := strings.Cut(l, "(")
				got = append(got, head)
			}
		}
	}

	if want := []string{"nodes = NODES", "OUTS"}; !cmp.Equal(got, want) {
		t.Errorf("the statements reading shared are %q, want %q", got, want)
	}
}
