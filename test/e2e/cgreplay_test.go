//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestMrpCallGraph runs each mrp-diff fixture under real mrp and replays its
// pipestance through the call-graph data plane: in DAG order, `mre args`
// builds every stage instance's args bundle from only what that instance
// stages (its fork elements and its parents' stamped outs), mrp's own _outs
// stand in for the stage, and `mre post` stamps them and writes the fork
// elements downstream instances fan out over; `mre outs` then resolves the
// pipeline outs. It requires:
//   - every instance's _args equal to mrp's after both are re-encoded and
//     file leaves are reduced to name and content digest, and its disabled
//     state to match mrp's _disabled marker;
//   - mrp's own resolver, run in-process on the same pipestance (the drift
//     guard), to resolve the same fork set and _args;
//   - the pipeline outs equal to mrp's, key order included.
func TestMrpCallGraph(t *testing.T) {
	t.Parallel()

	requireTools(t, "python3")

	bin := martianBin(t)
	buildBinaries(t)

	for _, tc := range mrpDiffCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shardSkip(t)

			tmp := t.TempDir()
			dir, mro := tc.source()
			runMrpAt(t, bin, dir, mro, tmp)
			newCGReplay(t, tc.name, dir, mro, tmp).run()
		})
	}
}

// cgReplay is one fixture's replay of its mrp pipestance.
type cgReplay struct {
	t       *testing.T
	fixture string
	g       *cg.Graph
	call    *syntax.CallGraphPipeline
	dir     string
	mro     string
	tmp     string
	ps      string
	types   string
	entry   string
	posted  map[string][]postedInstance
	ours    map[string]replayedFork
	// emptyFork holds the stages with a fork root that has no fork under
	// some fork of the roots before it.
	emptyFork map[string]bool
	seq       int
}

// postedInstance is one stage instance after `mre post`.
type postedInstance struct {
	fork  cg.Fork
	outs  string
	forks string
}

// replayedFork is what the replay resolved for one mrp fork dir.
type replayedFork struct {
	disabled bool
	args     json.RawMessage
}

// replayInstance is one stage instance to replay: the fork elements it
// stages, the fork they give it, and each part's fork count under the parts
// before it (forkDir's lens).
type replayInstance struct {
	elems []string
	fork  cg.Fork
	lens  []int
}

func newCGReplay(t *testing.T, fixture, dir, entry, tmp string) *cgReplay {
	t.Helper()

	mro := filepath.Join(dir, entry)

	g, err := cg.Load(mro, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	_, _, ast, err := syntax.Compile(mro, []string{dir}, false)
	if err != nil {
		t.Fatal(err)
	}

	call, err := ast.MakePipelineCallGraph("", ast.Call)
	if err != nil {
		t.Fatal(err)
	}

	rp := &cgReplay{
		t: t, fixture: fixture, g: g, call: call, dir: dir, mro: mro, tmp: tmp,
		ps:        filepath.Join(tmp, "mrp"),
		types:     filepath.Join(tmp, "types.json"),
		entry:     filepath.Join(tmp, "cg", "entry"),
		posted:    map[string][]postedInstance{},
		ours:      map[string]replayedFork{},
		emptyFork: map[string]bool{},
	}

	writeManifest(t, mro, dir, rp.types)

	return rp
}

func writeManifest(t *testing.T, mro, dir, path string) {
	t.Helper()

	ast, err := frontend.Parse(mro, []string{dir}, false)
	if err != nil {
		t.Fatal(err)
	}

	prog, err := frontend.Lower(ast, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	man := types.BuildManifest(prog)

	enc, err := man.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(enc.Subset(slices.Sorted(maps.Keys(man.Callables))...)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (rp *cgReplay) run() {
	rp.mre("post", "-entry", "-forks", rp.entry)

	ran, off := 0, 0

	for _, st := range rp.stageOrder() {
		for _, in := range rp.instances(st, 0, replayInstance{}) {
			if rp.replay(st, in) {
				off++
			} else {
				ran++
			}
		}
	}

	rp.checkCore()
	rp.checkPipelineOuts()
	rp.t.Logf("call graph replay: %d instance _args equal mrp's, %d instances disabled", ran, off)
}

// stageOrder lists the stage nodes so each follows the stages it reads and
// the producers of its fork roots.
func (rp *cgReplay) stageOrder() []*syntax.CallGraphStage {
	var ids []string

	for fqid, n := range rp.call.NodeClosure() {
		if _, ok := n.(*syntax.CallGraphStage); ok {
			ids = append(ids, fqid)
		}
	}

	slices.Sort(ids)

	done := map[string]bool{}

	var out []*syntax.CallGraphStage

	var visit func(string)

	visit = func(id string) {
		if done[id] {
			return
		}

		done[id] = true

		st, err := rp.g.Stage(id)
		if err != nil {
			rp.t.Fatal(err)
		}

		for _, d := range stageDeps(rp.g, st) {
			visit(d)
		}

		out = append(out, st)
	}

	for _, id := range ids {
		visit(id)
	}

	return out
}

func stageDeps(g *cg.Graph, st *syntax.CallGraphStage) []string {
	exps := make([]syntax.Exp, 0, len(st.ResolvedInputs())+len(st.Disabled()))

	for _, rb := range st.ResolvedInputs() {
		exps = append(exps, rb.Exp)
	}

	exps = append(exps, st.Disabled()...)

	set := map[string]bool{}

	for _, e := range exps {
		for _, r := range e.FindRefs() {
			set[r.Id] = true
		}
	}

	for _, r := range st.ForkRoots() {
		if p, ok := g.Producer(r); ok {
			set[p] = true
		}
	}

	return sortedKeys(set)
}

// instances enumerates st's instances root by root from the fork elements
// the root's feeder posted: its producer's instances, or the entry.
func (rp *cgReplay) instances(st *syntax.CallGraphStage, i int, prefix replayInstance) []replayInstance {
	roots := st.ForkRoots()
	if i == len(roots) {
		return []replayInstance{prefix}
	}

	var found []replayInstance

	for _, dir := range rp.elementDirs(roots[i]) {
		el, err := rp.g.ReadElement(dir)
		if err != nil {
			rp.t.Fatal(err)
		}

		if forkAgrees(el.Fork, prefix.fork) {
			found = append(found, replayInstance{
				elems: append(slices.Clip(prefix.elems), dir),
				fork:  append(slices.Clip(prefix.fork), cg.Part{Root: roots[i], Index: el.Fork[len(el.Fork)-1].Index}),
			})
		}
	}

	if len(found) == 0 {
		rp.emptyFork[st.Fqid] = true
	}

	var out []replayInstance

	for _, f := range found {
		f.lens = append(slices.Clip(prefix.lens), len(found))
		out = append(out, rp.instances(st, i+1, f)...)
	}

	return out
}

func (rp *cgReplay) elementDirs(root *syntax.CallGraphStage) []string {
	var feeders []string

	if p, ok := rp.g.Producer(root); ok {
		for _, pi := range rp.posted[p] {
			feeders = append(feeders, pi.forks)
		}
	} else {
		feeders = []string{rp.entry}
	}

	var out []string

	for _, f := range feeders {
		ms, err := filepath.Glob(filepath.Join(f, root.Fqid, "*"))
		if err != nil {
			rp.t.Fatal(err)
		}

		out = append(out, ms...)
	}

	return out
}

// forkAgrees reports whether two forks give every root they share the same
// index.
func forkAgrees(a, b cg.Fork) bool {
	for _, p := range a {
		for _, q := range b {
			if p.Root.Fqid == q.Root.Fqid && p.Index.GoString() != q.Index.GoString() {
				return false
			}
		}
	}

	return true
}

// replay runs one instance's args and post, checks its _args against mrp's,
// and reports whether it is disabled.
func (rp *cgReplay) replay(st *syntax.CallGraphStage, in replayInstance) bool {
	rp.seq++
	work := filepath.Join(rp.tmp, "cg", fmt.Sprintf("%04d", rp.seq))
	args, outs := filepath.Join(work, "args"), filepath.Join(work, "outs")

	staged := append(slices.Clone(in.elems), rp.upstreamOuts(st, in.fork)...)

	argv := make([]string, 0, 5+2*len(staged))
	argv = append(argv, "args", "-node", st.Fqid, "-o", args)

	for _, d := range staged {
		argv = append(argv, "-in", d)
	}

	state := strings.TrimSpace(rp.mre(argv...))

	inst, err := rp.g.ReadInstance(args)
	if err != nil {
		rp.t.Fatal(err)
	}

	if !forkAgrees(inst.Fork, in.fork) || len(inst.Fork) != len(in.fork) {
		rp.t.Errorf("%s: mre args chose fork %s, staged %s", st.Fqid, inst.Fork, in.fork)
	}

	rel := filepath.Join(strings.ReplaceAll(st.Fqid, ".", string(filepath.Separator)), forkDir(inst.Fork, in.lens))
	mrpArgs, mrpErr := os.ReadFile(filepath.Join(rp.ps, rel, "split", argsFile))

	if disabled := state == "disabled"; disabled != rp.mrpDisabled(rel) {
		rp.t.Errorf("%s: mre args says %s, mrp's run disagrees", rel, state)
	}

	if state == "disabled" {
		rp.ours[rel] = replayedFork{disabled: true}
	} else {
		got := rp.checkArgs(rel, args, mrpArgs, mrpErr)
		rp.ours[rel] = replayedFork{args: got}
		rp.writeOuts(st, filepath.Join(rp.ps, rel, outsFile), outs)
	}

	rp.mre("post", "-args", args, "-outs", outs, "-forks", filepath.Join(work, "forks"))
	rp.posted[st.Fqid] = append(rp.posted[st.Fqid], postedInstance{fork: inst.Fork, outs: outs, forks: filepath.Join(work, "forks")})

	return state == "disabled"
}

func (rp *cgReplay) checkArgs(rel, args string, want []byte, wantErr error) json.RawMessage {
	got, err := shim.ReadBundle(args)
	if err != nil {
		rp.t.Fatal(err)
	}

	if wantErr != nil {
		rp.t.Errorf("%s: mre runs a fork mrp has no _args for: %v", rel, wantErr)

		return got
	}

	if g, w := normJSON(rp.t, got, leafNamed), normJSON(rp.t, want, leafNamed); g != w {
		rp.t.Errorf("%s: _args differ from mrp's\nmrp:\n%s\nmre args:\n%s", rel, w, g)
	}

	return got
}

// upstreamOuts are the posted outs an instance stages: those of each stage
// its bindings read, at the forks that agree with the instance's, less a
// producer it reads only through the splits its fork elements serve.
func (rp *cgReplay) upstreamOuts(st *syntax.CallGraphStage, fork cg.Fork) []string {
	elemOnly := map[*syntax.CallStm]string{}

	for _, r := range st.ForkRoots() {
		if p, ok := rp.g.Producer(r); ok {
			elemOnly[r.Call()] = p
		}
	}

	read := map[string]bool{}

	for _, rb := range st.ResolvedInputs() {
		refsOutsideSplits(rb.Exp, elemOnly, read)
	}

	for _, d := range st.Disabled() {
		refsOutsideSplits(d, elemOnly, read)
	}

	var out []string

	for _, u := range sortedKeys(read) {
		for _, pi := range rp.posted[u] {
			if forkAgrees(pi.fork, fork) {
				out = append(out, pi.outs)
			}
		}
	}

	return out
}

// refsOutsideSplits collects the stages e reads other than through a split
// of a root's own producer.
func refsOutsideSplits(e syntax.Exp, elemOnly map[*syntax.CallStm]string, read map[string]bool) {
	switch x := e.(type) {
	case *syntax.SplitExp:
		if ref, ok := x.Value.(*syntax.RefExp); ok && elemOnly[x.Call] == ref.Id {
			return
		}

		refsOutsideSplits(x.Value, elemOnly, read)
	case *syntax.RefExp:
		read[x.Id] = true
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			refsOutsideSplits(v, elemOnly, read)
		}
	case *syntax.MapExp:
		for _, v := range x.Value {
			refsOutsideSplits(v, elemOnly, read)
		}
	case *syntax.MergeExp:
		refsOutsideSplits(x.Value, elemOnly, read)

		if x.ForkNode != nil {
			read[x.ForkNode.Id] = true
		}
	case *syntax.DisabledExp:
		refsOutsideSplits(x.Disabled, elemOnly, read)
		refsOutsideSplits(x.Value, elemOnly, read)
	}
}

// writeOuts writes the _outs mrp's stage wrote as the outs bundle the stage
// task would hand `mre post`.
func (rp *cgReplay) writeOuts(st *syntax.CallGraphStage, mrpOuts, dir string) {
	raw, err := os.ReadFile(mrpOuts)
	if err != nil {
		rp.t.Fatalf("mrp outs: %v", err)
	}

	man, err := types.LoadManifest(rp.types)
	if err != nil {
		rp.t.Fatal(err)
	}

	params, err := man.Params(st.Call().DecId, types.RoleOut)
	if err != nil {
		rp.t.Fatal(err)
	}

	payload := map[string]any{}

	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		v, err := types.DecodeJSON(raw)
		if err != nil {
			rp.t.Fatal(err)
		}

		obj, _ := types.AsObject(v)
		for _, m := range obj {
			payload[m.Key] = m.Value
		}
	}

	if err := shim.WriteBundle(dir, payload, params, man.Table(), nil); err != nil {
		rp.t.Fatal(err)
	}
}

// mrpDisabled reports whether mrp's run disabled the fork at rel: it writes
// the _disabled marker (core/stage.go:1001 Fork.skip).
func (rp *cgReplay) mrpDisabled(rel string) bool {
	_, err := os.Stat(filepath.Join(rp.ps, rel, "_disabled"))

	return err == nil
}

// checkCore is the drift guard: mrp's own resolver, reattached to the same
// pipestance, must hold exactly the forks the replay ran, plus only the
// disabled placeholder fork0 mrp keeps for a stage whose map call has no
// forks under some outer fork (an empty or null source), and resolve the
// same _args for every fork that runs.
func (rp *cgReplay) checkCore() {
	cf := coreForks(rp.t.Context(), rp.t, rp.ps, []string{rp.dir})

	for _, rel := range sortedKeys(rp.ours) {
		o := rp.ours[rel]

		c, ok := cf[rel]

		switch {
		case !ok:
			rp.t.Errorf("drift guard: %s: mrp's resolver has no such fork", rel)
		case o.disabled:
		case !c.ready:
			rp.t.Errorf("drift guard: %s: mrp's resolver is still waiting on its inputs", rel)
		default:
			if g, w := normJSON(rp.t, o.args, leafNamed), normJSON(rp.t, c.args, leafNamed); g != w {
				rp.t.Errorf("drift guard: %s: _args differ from mrp's resolver\nmrp resolver:\n%s\nmre args:\n%s", rel, w, g)
			}
		}
	}

	for _, rel := range sortedKeys(cf) {
		placeholder := filepath.Base(rel) == "fork0" && rp.mrpDisabled(rel) && rp.emptyFork[cf[rel].node]
		if _, ok := rp.ours[rel]; !ok && !placeholder {
			rp.t.Errorf("drift guard: %s: mrp's resolver runs a fork the replay has no instance for", rel)
		}
	}
}

// checkPipelineOuts resolves the pipeline outs with `mre outs` from the
// posted outs of the stages they read, lays them out with publish-layout,
// and requires the outs JSON mrp wrote, as TestMrpDiff compares it.
func (rp *cgReplay) checkPipelineOuts() {
	out := filepath.Join(rp.tmp, "cg", "pipeline_outs")
	argv := []string{"outs", "-o", out}

	if ro := rp.call.ResolvedOutputs(); ro != nil && ro.Exp != nil {
		for _, id := range refIDsOf(ro.Exp) {
			for _, pi := range rp.posted[id] {
				argv = append(argv, "-in", pi.outs)
			}
		}
	}

	rp.mre(argv...)

	pub := filepath.Join(rp.tmp, "cg", "published")
	if err := os.MkdirAll(pub, 0o755); err != nil {
		rp.t.Fatal(err)
	}

	rp.run1(filepath.Join(root, "mre"), "publish-layout", "-sidecar", filepath.Join(out, "data.json"), "-dir", pub,
		"-leaves", filepath.Join(out, "f"), "-types", rp.types, "-callable", rp.call.Call().DecId, "-role", types.RoleOut)

	allow := mustAllowlist(rp.t)

	want, err := mrpOutsJSON(allow, rp.fixture, rp.ps)
	if err != nil {
		rp.t.Fatal(err)
	}

	// The replay stages mrp's own stage outs, so an unpublished file's recorded
	// origin is its pipestance path: read it through the same rewrite as mrp's.
	rewrite, err := mrpOutsRewrite(allow, rp.fixture, filepath.Join(rp.ps, "outs"))
	if err != nil {
		rp.t.Fatal(err)
	}

	got, err := decodeOutsFile(filepath.Join(pub, "pipeline_outs.json"), rewrite)
	if err != nil {
		rp.t.Fatal(err)
	}

	w, err := encodeOrderedJSON(want)
	if err != nil {
		rp.t.Fatal(err)
	}

	g, err := encodeOrderedJSON(got)
	if err != nil {
		rp.t.Fatal(err)
	}

	if g != w {
		rp.t.Errorf("pipeline outs differ from mrp's (-mrp +mre outs):\n%s", cmp.Diff(indentJSON(w), indentJSON(g)))
	}
}

func refIDsOf(e syntax.Exp) []string {
	set := map[string]bool{}
	for _, r := range e.FindRefs() {
		set[r.Id] = true
	}

	return sortedKeys(set)
}

// mre runs one call-graph subcommand against the fixture's invocation, from
// mrp's working dir, and returns its stdout.
func (rp *cgReplay) mre(argv ...string) string {
	rp.t.Helper()

	return rp.run1(filepath.Join(root, "mre"), append([]string{argv[0], "-mro", rp.mro, "-mropath", rp.dir, "-types", rp.types}, argv[1:]...)...)
}

// run1 runs one command from mrp's working dir and returns its stdout.
func (rp *cgReplay) run1(bin string, argv ...string) string {
	rp.t.Helper()

	cmd := exec.Command(bin, argv...)
	cmd.Dir = rp.tmp

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			rp.t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(argv, " "), err, stderr.String())
		}

		rp.t.Fatal(err)
	}

	return stdout.String()
}
