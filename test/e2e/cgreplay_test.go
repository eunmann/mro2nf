//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestMrpCallGraph runs each mrp-diff fixture under real mrp and replays its
// pipestance through the call-graph data plane: in DAG order, every stage
// instance runs its tasks through `mre stage` as a local run's tasks do,
// reading host entry inputs in place, from only
// what the instance stages (its fork elements and its parents' posted
// outs): each task builds the instance's args, the task that runs its
// first job reporting it disabled, each task runs its phase's job through
// mrjob, and the task that runs its last job posts its outs and the fork
// elements downstream instances fan out over; `mre outs` then resolves the
// pipeline outs. It requires:
//   - every instance's _args, resolved in-process from the same items,
//     equal to mrp's after both are re-encoded and
//     file leaves are reduced to name and content digest, and its disabled
//     state, which `mre stage` reports by refusing the instance before any
//     job runs, to match mrp's _disabled marker;
//   - every job mrp ran, and only those, run with the same _args and _outs,
//     a file under the job's own files dir named relative to it, and the
//     same _jobinfo: the keys, and the values of jobInfoStable;
//   - mrp's own resolver, run in-process on the same pipestance (the drift
//     guard), to resolve the same fork set and _args;
//   - the published tree and pipeline outs equal to mrp's, as TestMrpDiff
//     compares them.
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
			mrpFlags, _ := tc.flags()
			runMrpAt(t, bin, dir, mro, tmp, mrpFlags...)

			rp := newCGReplay(t, tc.name, dir, mro, tmp, filepath.Join(filepath.Dir(bin), "adapters"))
			if tc.overrides != "" {
				rp.overrides = filepath.Join(dir, tc.overrides)
			}

			rp.run()
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
	entry   string
	// adapters is mrp's adapters dir, which `mre stage` runs py stages with.
	adapters string
	// overrides is the mrp --overrides file mrp ran with, which every job
	// runs with too.
	overrides string
	posted    map[string][]postedInstance
	ours      map[string]replayedFork
	// refs maps each dir a post wrote to the ordinal dirs of the files it
	// names by reference, where their producers wrote them, as the
	// driver's POSTED resolves them.
	refs map[string][]string
	// placeholderDirs holds the dirs, relative to the pipestance, of each
	// fork of a stage whose fork root has no fork under the forks of the
	// roots before it, where mrp keeps a disabled placeholder.
	placeholderDirs map[string]bool
	// off holds the stage nodes the topology disables outright, whose
	// forks mrp does not expand from a stage's outs.
	off map[string]bool
	// zips are each stage node's splits of a stage output it gets the
	// partner element of, which its producer posts in place of the output.
	zips map[string][]cg.Zip
	seq  int
	jobs int
}

// postedInstance is one stage instance its last task posted: its outs and
// the elements it feeds, each as dirs and as value records.
type postedInstance struct {
	fork                                cg.Fork
	outs, outsValues, forks, forkValues string
}

// replayedFork is what the replay resolved for one mrp fork dir.
type replayedFork struct {
	disabled bool
	args     json.RawMessage
}

// replayInstance is one stage instance to replay: the fork element items
// it gets, dirs it stages and value records, and the fork they give it.
type replayInstance struct {
	elems []string
	fork  cg.Fork
	lens  []int
}

// replayTasks are the files an instance's tasks share below its dir work:
// the dir the first task links its args' files below, the split's chunks
// dir and the references its chunk defs hold, each chunk's outs bundle,
// the outs and fork elements the last task posts, as dirs, value records
// and references; the files the instance stages by reference, each linked
// as the driver stages them; the items every task of the instance gets to
// build its own args, and in, their flags.
type replayTasks struct {
	work, args, chunks, chunkRefs, chunkOuts, refs          string
	outs, outsValues, outsRefs, forks, forkValues, forkRefs string
	items, in                                               []string
}

func newReplayTasks(work string) replayTasks {
	return replayTasks{
		work:       work,
		args:       filepath.Join(work, "args"),
		chunks:     filepath.Join(work, "chunks"),
		chunkRefs:  filepath.Join(work, "chunk_refs"),
		chunkOuts:  filepath.Join(work, "chunkouts"),
		refs:       filepath.Join(work, "refs"),
		outs:       filepath.Join(work, "outs"),
		outsValues: filepath.Join(work, "outs_values"),
		outsRefs:   filepath.Join(work, "outs_refs"),
		forks:      filepath.Join(work, "forks"),
		forkValues: filepath.Join(work, "fork_values"),
		forkRefs:   filepath.Join(work, "fork_refs"),
	}
}

// posting are the flags of the task that posts the instance's outs.
func (t replayTasks) posting() []string {
	return []string{
		"-fields", t.outs, "-field-values", t.outsValues, "-field-refs", t.outsRefs,
		"-forks", t.forks, "-fork-values", t.forkValues, "-fork-refs", t.forkRefs,
	}
}

// notEnabled is the message of the error `mre stage` exits 1 with when the
// instance its args step resolves is disabled.
const notEnabled = "not enabled where its gates said enabled"

func newCGReplay(t *testing.T, fixture, dir, entry, tmp, adapters string) *cgReplay {
	t.Helper()

	mro := filepath.Join(dir, entry)

	g, err := cg.Load(mro, "", []string{dir})
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
		ps:              filepath.Join(tmp, "mrp"),
		entry:           filepath.Join(tmp, "cg", "entry"),
		adapters:        adapters,
		posted:          map[string][]postedInstance{},
		ours:            map[string]replayedFork{},
		refs:            map[string][]string{},
		placeholderDirs: map[string]bool{},
		off:             map[string]bool{},
		zips:            map[string][]cg.Zip{},
	}

	topo, err := g.Topology()
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range topo.Nodes {
		rp.off[n.Fqid] = n.Off
		rp.zips[n.Fqid] = n.Zips
	}

	return rp
}

func (rp *cgReplay) run() {
	if err := rp.g.PostEntry(rp.entry); err != nil {
		rp.t.Fatal(err)
	}

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
	rp.t.Logf("call graph replay: %d instance _args equal mrp's, %d instances disabled, %d jobs run through mre stage", ran, off, rp.jobs)
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

// placeholders are the dirs mrp may keep its disabled placeholder of st's
// fork with no fork of root under prefix in: the id of prefix's parts, as
// an empty part ends a fork id (core/fork.go:1133), or, under no prefix,
// fork0 or the node's own dir.
func (rp *cgReplay) placeholders(st *syntax.CallGraphStage, root *syntax.CallGraphStage, prefix replayInstance) []string {
	dir := filepath.Join(strings.Split(st.Fqid, ".")...)
	if len(prefix.fork) == 0 {
		return []string{dir, filepath.Join(dir, "fork0")}
	}

	empty := append(slices.Clip(prefix.fork), cg.Part{Root: root, Index: cg.ArrayIndex(0)})

	return []string{filepath.Join(dir, empty.ID(append(slices.Clip(prefix.lens), 0)))}
}

// instances enumerates st's instances root by root from the fork elements
// the root's feeder posted: its producer's instances, or the entry.
func (rp *cgReplay) instances(st *syntax.CallGraphStage, i int, prefix replayInstance) []replayInstance {
	roots := st.ForkRoots()
	if i == len(roots) {
		return []replayInstance{prefix}
	}

	var found []replayInstance

	for _, item := range rp.elementItems(roots[i]) {
		el := rp.element(item)

		if forkAgrees(el.Fork, prefix.fork) {
			found = append(found, replayInstance{
				elems: append(slices.Clip(prefix.elems), item),
				fork:  append(slices.Clip(prefix.fork), cg.Part{Root: roots[i], Index: el.Fork[len(el.Fork)-1].Index}),
				lens:  append(slices.Clip(prefix.lens), el.Lens[len(el.Lens)-1]),
			})
		}
	}

	if len(found) == 0 {
		for _, rel := range rp.placeholders(st, roots[i], prefix) {
			rp.placeholderDirs[rel] = true
		}
	}

	var out []replayInstance

	for _, f := range found {
		out = append(out, rp.instances(st, i+1, f)...)
	}

	return out
}

// elementItems are root's fork element items its feeder posted: element
// dirs and value records of its producer's instances, or the entry's
// element dirs.
func (rp *cgReplay) elementItems(root *syntax.CallGraphStage) []string {
	var feeders []postedInstance

	if p, ok := rp.g.Producer(root); ok {
		feeders = rp.posted[p]
	} else {
		feeders = []postedInstance{{forks: rp.entry}}
	}

	var out []string

	for _, f := range feeders {
		ms, err := filepath.Glob(filepath.Join(f.forks, root.Fqid, "*"))
		if err != nil {
			rp.t.Fatal(err)
		}

		out = append(out, ms...)

		if f.forkValues == "" {
			continue
		}

		raw, err := os.ReadFile(f.forkValues)
		if err != nil {
			rp.t.Fatal(err)
		}

		for rec := range strings.FieldsSeq(string(raw)) {
			if strings.HasPrefix(rec, root.Fqid+"/") {
				out = append(out, rec)
			}
		}
	}

	return out
}

// element is the fork element item names: a dir, or a value record.
func (rp *cgReplay) element(item string) cg.Element {
	if filepath.IsAbs(item) {
		el, err := rp.g.ReadElement(item)
		if err != nil {
			rp.t.Fatal(err)
		}

		return el
	}

	el, ok, err := rp.g.ValueElement(item)
	if err != nil || !ok {
		rp.t.Fatalf("value record %.60q is no fork element: %v", item, err)
	}

	return el
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

// replay runs one instance's tasks, checks its _args and jobs against
// mrp's, and reports whether it is disabled, which its first task reports
// by refusing it before its job runs: as the driver runs no task of a
// disabled instance, it posts nothing.
func (rp *cgReplay) replay(st *syntax.CallGraphStage, in replayInstance) bool {
	rp.seq++
	tasks := newReplayTasks(rp.taskDir(fmt.Sprintf("instance %04d", rp.seq)))
	items := slices.Concat(in.elems, rp.partners(st, in.fork), rp.upstreamOuts(st, in.fork))
	rp.stageRefs(tasks.refs, items)
	tasks.items, tasks.in = items, rp.itemFlags(tasks.work, items)

	name, disabled := rp.firstTask(st, tasks)
	inst, args := rp.resolve(st, items, tasks.refs)

	if !forkAgrees(inst.Fork, in.fork) || len(inst.Fork) != len(in.fork) {
		rp.t.Errorf("%s: mre stage chose fork %s, staged %s", st.Fqid, inst.Fork, in.fork)
	}

	rel := filepath.Join(strings.ReplaceAll(st.Fqid, ".", string(filepath.Separator)), inst.ForkID())

	if mrpOff := rp.mrpDisabledAt(st, rel); disabled != mrpOff || disabled != inst.Disabled {
		rp.t.Errorf("%s: mre stage refused it as disabled: %t, its args resolve it disabled: %t, mrp's run disabled: %t",
			rel, disabled, inst.Disabled, mrpOff)
	}

	if disabled {
		if _, err := os.Lstat(filepath.Join(rp.jobDir(tasks, name), name)); !errors.Is(err, os.ErrNotExist) {
			rp.t.Errorf("%s: a disabled instance has a job dir: %v", rel, err)
		}

		rp.ours[rel] = replayedFork{disabled: true}

		return true
	}

	mrpArgs, mrpErr := os.ReadFile(filepath.Join(rp.ps, rel, "split", argsFile))
	rp.ours[rel] = replayedFork{args: rp.checkArgs(rel, args, mrpArgs, mrpErr)}
	rp.runStage(st, rel, tasks)
	rp.posted[st.Fqid] = append(rp.posted[st.Fqid], postedInstance{
		fork: inst.Fork, outs: tasks.outs, outsValues: tasks.outsValues, forks: tasks.forks, forkValues: tasks.forkValues,
	})
	rp.indexRefs(filepath.Dir(tasks.outs), tasks.outsRefs)
	rp.indexRefs(tasks.forks, tasks.forkRefs)

	return false
}

// indexRefs records the files the dirs a post wrote below base name by
// reference, the records of the file at path (mre stage -field-refs,
// -fork-refs), each at its ordinal dir in the item it lies in, which a
// task the replay runs is given by the path it lies at, as the driver
// resolves them against the paths it staged the item from.
func (rp *cgReplay) indexRefs(base, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		rp.t.Fatal(err)
	}

	for rec := range strings.SplitSeq(string(raw), " ") {
		if rec == "" {
			continue
		}

		f := strings.Split(rec, "\t")
		if len(f) != 3 {
			rp.t.Fatalf("%s: malformed reference %q", path, rec)
		}

		item := filepath.Join(base, f[0])
		rp.refs[item] = append(rp.refs[item], filepath.Join(f[1], f[2]))
	}
}

// stageRefs links the ordinal dir of each file the dirs among items name
// by reference that lies in none of them into dir, once each, as a task
// stages them, <dir>/<n>/f/<ordinal>, and returns them, the n-th first.
func (rp *cgReplay) stageRefs(dir string, items []string) []string {
	var ordinals []string

	for _, it := range items {
		for _, o := range rp.refs[it] {
			if !slices.Contains(ordinals, o) && !slices.Contains(items, filepath.Dir(filepath.Dir(o))) {
				ordinals = append(ordinals, o)
			}
		}
	}

	for i, o := range ordinals {
		at := filepath.Join(dir, strconv.Itoa(i+1), "f")
		if err := os.MkdirAll(at, 0o755); err != nil {
			rp.t.Fatal(err)
		}

		if err := os.Symlink(o, filepath.Join(at, filepath.Base(o))); err != nil {
			rp.t.Fatal(err)
		}
	}

	return ordinals
}

// resolve resolves st's instance from the items its tasks get, dirs and
// value records, and the files they name by reference staged in refs, and
// its args, as each task's args step does under -host-in-place, its files
// linked below a dir of the replay's own: no task writes its instance or
// args but into its jobs.
func (rp *cgReplay) resolve(st *syntax.CallGraphStage, items []string, refs string) (cg.Instance, json.RawMessage) {
	var in, values []string

	for _, it := range items {
		if filepath.IsAbs(it) {
			in = append(in, it)
		} else {
			values = append(values, it)
		}
	}

	roots, err := rp.g.Roots(cg.ReadInPlace{Host: true})
	if err != nil {
		rp.t.Fatal(err)
	}

	out := filepath.Join(rp.tmp, "cg", "resolved", strconv.Itoa(rp.seq))

	staged, err := shim.NewStaged(append(slices.Clone(in), refs)...)
	if err != nil {
		rp.t.Fatal(err)
	}

	leaves, err := staged.Leaves()
	if err != nil {
		rp.t.Fatal(err)
	}

	inst, args, err := rp.g.WriteArgs(cg.ArgsSpec{Node: st.Fqid, In: in, Values: values, Refs: leaves, Out: out, Roots: roots})
	if err != nil {
		rp.t.Fatalf("%s: resolve its instance: %v", st.Fqid, err)
	}

	raw, err := json.Marshal(args)
	if err != nil {
		rp.t.Fatal(err)
	}

	return inst, raw
}

// itemFlags are the args step's flags naming items, as a task gets them: a
// dir it stages as -in, and the value records, which it writes into one
// file below work, as -values.
func (rp *cgReplay) itemFlags(work string, items []string) []string {
	var (
		flags  []string
		values []string
	)

	for _, it := range items {
		if filepath.IsAbs(it) {
			flags = append(flags, "-in", it)
		} else {
			values = append(values, it)
		}
	}

	if len(values) == 0 {
		return flags
	}

	if err := os.MkdirAll(work, 0o755); err != nil {
		rp.t.Fatal(err)
	}

	file := filepath.Join(work, "values")
	if err := os.WriteFile(file, []byte(strings.Join(values, "\n")), 0o644); err != nil {
		rp.t.Fatal(err)
	}

	return append(flags, "-values", file)
}

// firstTask runs the instance's first task, which builds its args from the
// items it stages, and reports its job's metadata dir name and
// whether mre stage refused the instance as disabled: a split, or a
// non-split stage's main, which posts its outs too.
func (rp *cgReplay) firstTask(st *syntax.CallGraphStage, tasks replayTasks) (string, bool) {
	argv := slices.Concat([]string{"-node", st.Fqid, "-args", tasks.args, "-refs", tasks.refs}, tasks.in)

	if s, ok := st.Callable().(*syntax.Stage); ok && s.Split {
		return job.PhaseSplit, rp.stage(job.PhaseSplit, rp.jobDir(tasks, job.PhaseSplit), append(argv, "-o", tasks.chunks, "-out-refs", tasks.chunkRefs)...)
	}

	name := job.ChunkName(0, 1)

	return name, rp.stage(job.PhaseMain, rp.jobDir(tasks, name), append(argv, tasks.posting()...)...)
}

func (rp *cgReplay) checkArgs(rel string, got json.RawMessage, want []byte, wantErr error) json.RawMessage {
	if wantErr != nil {
		rp.t.Errorf("%s: mre stage runs a fork mrp has no _args for: %v", rel, wantErr)

		return got
	}

	// mrp leaves a fork's split/_args empty when a split reads a key its
	// map lacks (zip_runtime_map BY_AX/fork_x); the job's own _args, which
	// checkJob compares, carry the bindings, and the drift guard compares
	// them with mrp's resolver.
	if len(bytes.TrimSpace(want)) == 0 {
		return got
	}

	if g, w := normJSON(rp.t, got, leafNamed), normJSON(rp.t, want, leafNamed); g != w {
		rp.t.Errorf("%s: _args differ from mrp's\nmrp:\n%s\nmre stage:\n%s", rel, w, g)
	}

	return got
}

// upstreamOuts are the posted outs an instance stages: those of each stage
// its bindings read, at the forks that agree with the instance's, less a
// producer it reads only through splits of a root's own source, which its
// fork elements serve. A split of any other output resolves from the outs.
func (rp *cgReplay) upstreamOuts(st *syntax.CallGraphStage, fork cg.Fork) []string {
	elemOnly := map[*syntax.CallStm]string{}

	for _, r := range st.ForkRoots() {
		own, isRef := r.Split().Value.(*syntax.RefExp)
		if p, ok := rp.g.Producer(r); ok && isRef {
			elemOnly[r.Call()] = p + "." + own.OutputId
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
				out = append(append(out, rp.fields(pi)...), rp.records(pi.outsValues)...)
			}
		}
	}

	return out
}

// partners are the partner elements st's instance at fork gets, as the
// driver's ZIP gives them: of each output it zips, the element its
// producer's instance agreeing with fork posted at fork's index of the
// zip's root, or the output's absent record where it posted none there.
func (rp *cgReplay) partners(st *syntax.CallGraphStage, fork cg.Fork) []string {
	var out []string

	for _, z := range rp.zips[st.Fqid] {
		at := slices.IndexFunc(fork, func(p cg.Part) bool { return p.Root.Fqid == z.Root })
		if at < 0 {
			rp.t.Fatalf("%s zips %s.%s over %s, which its fork %v lacks", st.Fqid, z.Node, z.Output, z.Root, fork)
		}

		dir := cg.PartnerDir(z.Root, z.Node, z.Output)

		for _, pi := range rp.posted[z.Node] {
			if forkAgrees(pi.fork, fork) {
				out = append(out, rp.partner(pi, dir, cg.IndexToken(fork[at].Index)))
			}
		}
	}

	return out
}

// partner is the partner element pi posted in dir named token: its element
// dir, its value record, or else the dir's absent record.
func (rp *cgReplay) partner(pi postedInstance, dir, token string) string {
	if el := filepath.Join(pi.forks, dir, token); fileExists(el) {
		return el
	}

	recs := rp.records(pi.forkValues)

	for _, name := range []string{token, cg.AbsentName} {
		if i := slices.IndexFunc(recs, func(r string) bool { return strings.HasPrefix(r, dir+"/"+name+":") }); i >= 0 {
			return recs[i]
		}
	}

	rp.t.Fatalf("%s posted no partner element %s/%s and no absent record", pi.outs, dir, token)

	return ""
}

// fields are the output bundles the instance posted.
func (rp *cgReplay) fields(pi postedInstance) []string {
	dirs, err := filepath.Glob(filepath.Join(pi.outs, "*"))
	if err != nil {
		rp.t.Fatal(err)
	}

	return dirs
}

// records are the value records a post wrote to the file at path.
func (rp *cgReplay) records(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		rp.t.Fatal(err)
	}

	return strings.Fields(string(raw))
}

// refsOutsideSplits collects the stages e reads other than through a split
// of a root's own source, elemOnly's <producer>.<output> by the root's call.
func refsOutsideSplits(e syntax.Exp, elemOnly map[*syntax.CallStm]string, read map[string]bool) {
	switch x := e.(type) {
	case *syntax.SplitExp:
		if ref, ok := x.Value.(*syntax.RefExp); ok && elemOnly[x.Call] == ref.Id+"."+ref.OutputId {
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

		// A disabled instance posts no outs, so a merge over a root whose
		// forks a stage fixes counts them from the root's source, as the
		// run's tasks do (cg.Topology).
		if sp := x.Call.Split(); sp != nil && !sp.Source.KnownLength() {
			refsOutsideSplits(sp.Value, nil, read)
		}
	case *syntax.DisabledExp:
		refsOutsideSplits(x.Disabled, elemOnly, read)
		refsOutsideSplits(x.Value, elemOnly, read)
	}
}

// jobInfoStable are the _jobinfo keys whose values are the same every run;
// every other key (cwd, host, pid, rusage, ...) varies per run, so only its
// presence is compared.
var jobInfoStable = []string{
	"name", "type", "threads", "memGB", "vmemGB", "profile_mode",
	"stackvars_flag", "monitor_flag", "invocation", "version",
}

// runStage checks the job the instance's first task ran against the job mrp
// ran for the fork at rel, and runs and checks the rest: a stage that does
// not split runs one chunk, whose task posts its outs, and a split stage its
// split, a task per chunk the split returns, and its join, whose task posts
// them.
func (rp *cgReplay) runStage(st *syntax.CallGraphStage, rel string, tasks replayTasks) {
	if s, ok := st.Callable().(*syntax.Stage); !ok || !s.Split {
		for _, phase := range []string{job.PhaseSplit, job.PhaseJoin} {
			if regularFileExists(filepath.Join(rp.ps, rel, phase, "_jobinfo")) {
				rp.t.Errorf("%s: mrp ran a %s job for a stage that does not split", rel, phase)
			}
		}

		rp.checkJob(rel, job.ChunkName(0, 1), tasks)

		return
	}

	rp.checkJob(rel, job.PhaseSplit, tasks)

	ours, theirs := rp.chunkNames(tasks.chunks), rp.chunkNames(filepath.Join(rp.ps, rel))
	if !slices.Equal(ours, theirs) {
		rp.t.Errorf("%s: the split returned chunks %v, mrp's %v", rel, ours, theirs)
	}

	rp.indexRefs(tasks.work, tasks.chunkRefs)

	// Each task builds its own args bundle in its work dir from the items
	// the split staged, and stages the files those and the split's bundles
	// it stages, staged, name by reference.
	node := func(name string, staged ...string) []string {
		refs := filepath.Join(rp.jobDir(tasks, name), "refs")
		rp.stageRefs(refs, slices.Concat(tasks.items, staged))

		return slices.Concat([]string{"-node", st.Fqid, "-args", filepath.Join(rp.jobDir(tasks, name), "args"), "-refs", refs}, tasks.in)
	}

	var staged []string

	for _, c := range ours {
		def, outs, refs := filepath.Join(tasks.chunks, c), filepath.Join(tasks.chunkOuts, c), filepath.Join(rp.jobDir(tasks, c), "chunkout_refs")
		rp.runTask(rel, c, job.PhaseMain, tasks, slices.Concat(node(c, def), []string{"-chunk", def, "-o", outs, "-out-refs", refs})...)
		rp.indexRefs(tasks.work, refs)

		staged = append(staged, def, outs)
	}

	rp.runTask(rel, job.PhaseJoin, job.PhaseJoin, tasks,
		slices.Concat(node(job.PhaseJoin, staged...), []string{"-chunks", tasks.chunks, "-chunkouts", tasks.chunkOuts}, tasks.posting())...)
}

func (rp *cgReplay) chunkNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		rp.t.Fatal(err)
	}

	var out []string

	for _, e := range entries {
		// chnkN is mrp's link to its uniquified chunk dir, chnkN-u<id>.
		if chunkDir.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}

	return out
}

// taskDir is the work dir of the task id names, laid out as a Nextflow
// task's, work/<2 hex>/<30 hex>, as the unpublished-file-paths allowlist
// rule recognizes it.
func (rp *cgReplay) taskDir(id string) string {
	sum := sha256.Sum256([]byte(id))
	hash := hex.EncodeToString(sum[:])

	return filepath.Join(rp.tmp, "work", hash[:2], hash[2:32])
}

// jobDir is the work dir of the instance's task whose job's metadata dir is
// name, its own as each task's is.
func (rp *cgReplay) jobDir(tasks replayTasks, name string) string {
	return rp.taskDir(tasks.work + "\x00" + name)
}

// stage runs one `mre stage` task of phase in the work dir jw and reports
// whether mre refused its instance as disabled: it exits 1 with the
// message of its not-enabled error. Any other failure fails the test.
func (rp *cgReplay) stage(phase, jw string, argv ...string) bool {
	rp.t.Helper()

	base := []string{
		"stage", "-phase", phase, "-psid", "mrp", "-adapters", rp.adapters, "-mrjob", filepath.Join(root, "mrjob"), "-work", jw,
		"-local-cores", strconv.Itoa(mrpLocalCores), "-local-mem-gb", strconv.Itoa(mrpLocalMemGB), "-host-in-place",
	}
	if rp.overrides != "" {
		base = append(base, "-overrides", rp.overrides)
	}

	stderr, err := rp.mre(slices.Concat(base, argv)...)

	var exit *exec.ExitError

	switch {
	case err == nil:
		return false
	case errors.As(err, &exit) && exit.ExitCode() == 1 && strings.Contains(stderr, notEnabled):
		return true
	default:
		rp.t.Fatalf("%v\n%s", err, stderr)

		return false
	}
}

// runTask runs one task of an instance that its first task found enabled,
// whose job's metadata dir is name, and checks the job.
func (rp *cgReplay) runTask(rel, name, phase string, tasks replayTasks, argv ...string) {
	if rp.stage(phase, rp.jobDir(tasks, name), argv...) {
		rp.t.Fatalf("%s: mre stage refused its %s as disabled", rel, name)
	}

	rp.checkJob(rel, name, tasks)
}

// checkJob checks the metadata of the instance's job name, which its task
// ran, against mrp's job of that name at rel.
func (rp *cgReplay) checkJob(rel, name string, tasks replayTasks) {
	rp.jobs++

	ours, theirs := filepath.Join(rp.jobDir(tasks, name), name), filepath.Join(rp.ps, rel, name)
	for _, f := range []string{argsFile, outsFile} {
		rp.checkJobFile(rel+"/"+name, f, ours, theirs)
	}

	rp.checkJobInfo(rel+"/"+name, ours, theirs)
}

// checkJobFile requires the job file f to be in both jobs' metadata or in
// neither, and equal once a path under the job's own files dir is named
// relative to it.
func (rp *cgReplay) checkJobFile(job, f, ours, theirs string) {
	o, oerr := os.ReadFile(filepath.Join(ours, f))
	m, merr := os.ReadFile(filepath.Join(theirs, f))

	switch {
	case oerr != nil && merr != nil:
		return
	case oerr != nil || merr != nil:
		rp.t.Errorf("%s: %s: mre stage %v, mrp %v", job, f, oerr, merr)

		return
	}

	if g, w := rp.normJob(o, ours), rp.normJob(m, theirs); g != w {
		rp.t.Errorf("%s: %s differs from mrp's (-mrp +mre stage):\n%s", job, f, cmp.Diff(w, g))
	}
}

// normJob normalizes a job file as normJSON does, then names a path under
// the job's files dir, which the stage never wrote, relative to it.
func (rp *cgReplay) normJob(raw []byte, meta string) string {
	out := normJSON(rp.t, raw, leafNamed)

	files := []string{filepath.Join(meta, "files")}
	if resolved, err := filepath.EvalSymlinks(meta); err == nil {
		files = append(files, filepath.Join(resolved, "files"))
	}

	for _, f := range files {
		out = strings.ReplaceAll(out, `"`+f+`/`, `"<files>/`)
	}

	return out
}

// checkJobInfo requires both jobs' _jobinfo to hold the same keys, and the
// same values of jobInfoStable.
func (rp *cgReplay) checkJobInfo(job, ours, theirs string) {
	o, m := rp.readJobInfo(ours), rp.readJobInfo(theirs)

	if g, w := sortedKeys(o), sortedKeys(m); !slices.Equal(g, w) {
		rp.t.Errorf("%s: _jobinfo keys differ from mrp's (-mrp +mre stage):\n%s", job, cmp.Diff(w, g))
	}

	for _, k := range jobInfoStable {
		if g, w := normJSON(rp.t, orNull(o[k]), leafNamed), normJSON(rp.t, orNull(m[k]), leafNamed); g != w {
			rp.t.Errorf("%s: _jobinfo %s differs from mrp's (-mrp +mre stage):\n%s", job, k, cmp.Diff(w, g))
		}
	}
}

func (rp *cgReplay) readJobInfo(meta string) map[string]json.RawMessage {
	raw, err := os.ReadFile(filepath.Join(meta, "_jobinfo"))
	if err != nil {
		rp.t.Fatal(err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		rp.t.Fatalf("decode %s/_jobinfo: %v", meta, err)
	}

	return m
}

func orNull(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return json.RawMessage("null")
	}

	return raw
}

// mrpDisabled reports whether mrp's run disabled the fork at rel: it writes
// the _disabled marker (core/stage.go:1001 Fork.skip).
// mrpDisabledAt reports whether mrp's run disabled st's fork at rel: its
// dir holds _disabled, or, for a node disabled outright whose forks a
// stage's outs fix, mrp expanded none and kept only its disabled
// placeholder (merge_sub_root's P0.ST1), where the driver drops every fork
// the outs give it.
func (rp *cgReplay) mrpDisabledAt(st *syntax.CallGraphStage, rel string) bool {
	if rp.mrpDisabled(rel) {
		return true
	}

	if _, err := os.Stat(filepath.Join(rp.ps, rel)); !rp.off[st.Fqid] || !errors.Is(err, os.ErrNotExist) {
		return false
	}

	dir := filepath.Join(strings.Split(st.Fqid, ".")...)

	return rp.mrpDisabled(dir) || rp.mrpDisabled(filepath.Join(dir, "fork0"))
}

func (rp *cgReplay) mrpDisabled(rel string) bool {
	_, err := os.Stat(filepath.Join(rp.ps, rel, "_disabled"))

	return err == nil
}

// checkCore is the drift guard: mrp's own resolver, reattached to the same
// pipestance, must hold exactly the forks the replay ran, plus only the
// disabled placeholder mrp keeps for a stage whose map call has no forks
// under some outer fork (an empty or null source), in a placeholder dir,
// and resolve the same _args for every fork that runs.
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
				rp.t.Errorf("drift guard: %s: _args differ from mrp's resolver\nmrp resolver:\n%s\nmre stage:\n%s", rel, w, g)
			}
		}
	}

	for _, rel := range sortedKeys(cf) {
		placeholder := rp.placeholderDirs[rel] && rp.mrpDisabled(rel)
		if _, ok := rp.ours[rel]; !ok && !placeholder {
			rp.t.Errorf("drift guard: %s: mrp's resolver runs a fork the replay has no instance for", rel)
		}
	}
}

// checkPipelineOuts publishes the pipeline outs with `mre outs` from the
// skeletons of the posted outs of the stages the OUTS task stages, those
// the topology's outs read, and of the bundles holding the files they name
// by reference, each located where it was posted, as OUTS stages them
// (#610). It publishes the manifest as the workflow outputs do and
// requires the tree and outs JSON mrp published, as TestMrpDiff compares
// them, with the outs JSON at the root of the tree as OUTS publishes it.
func (rp *cgReplay) checkPipelineOuts() {
	results := filepath.Join(rp.tmp, "cg", "results")
	locations := filepath.Join(rp.tmp, "cg", "locations")
	manifest := filepath.Join(rp.tmp, "cg", "publish", "publish.json")
	valuesFile := filepath.Join(rp.tmp, "cg", "outs_values")
	in, refs := filepath.Join(rp.tmp, "cg", "outs-in"), filepath.Join(rp.tmp, "cg", "outs-refs")

	items, values, lines := rp.stageOutsItems(in)
	lines += rp.stageRefSkeletons(refs, items)

	for path, body := range map[string]string{locations: lines, valuesFile: strings.Join(values, "\n")} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			rp.t.Fatal(err)
		}
	}

	argv := []string{
		"outs", "-locations", locations, "-work", filepath.Join(rp.tmp, "cg", "pipestance"), "-values", valuesFile,
		"-manifest", manifest, "-json", filepath.Join(results, "pipeline_outs.json"), "-in", in, "-refs", refs,
	}

	if stderr, err := rp.mre(argv...); err != nil {
		rp.t.Fatalf("%v\n%s", err, stderr)
	}

	publishManifest(rp.t, manifest, results)

	reports, err := diffPublished(mustAllowlist(rp.t), rp.fixture, rp.ps, results)
	if err != nil {
		rp.t.Fatal(err)
	}

	for _, r := range reports {
		rp.t.Error(r)
	}
}

// stageOutsItems stages into in the skeleton of each outs bundle the
// topology's outs read, as in/u<i>, and returns the bundles, the value
// records, and the locations lines of the skeletons.
func (rp *cgReplay) stageOutsItems(in string) ([]string, []string, string) {
	rp.t.Helper()

	topo, err := rp.g.Topology()
	if err != nil {
		rp.t.Fatal(err)
	}

	if err := os.MkdirAll(in, 0o755); err != nil {
		rp.t.Fatal(err)
	}

	var lines strings.Builder

	var values, items []string

	for _, r := range topo.Outs {
		for _, pi := range rp.posted[r.Node] {
			for _, d := range rp.fields(pi) {
				items = append(items, d)
				lines.WriteString(rp.stageSkeleton(filepath.Join(in, "u"+strconv.Itoa(len(items))), d))
			}

			values = append(values, rp.records(pi.outsValues)...)
		}
	}

	return items, values, lines.String()
}

// stageRefSkeletons stages into refs, as refs/<i>, the skeleton of each
// bundle holding a file items name by reference that no item is, once,
// and returns their locations lines.
func (rp *cgReplay) stageRefSkeletons(refs string, items []string) string {
	rp.t.Helper()

	if err := os.MkdirAll(refs, 0o755); err != nil {
		rp.t.Fatal(err)
	}

	var lines strings.Builder

	var bundles []string

	for _, it := range items {
		for _, o := range rp.refs[it] {
			// A replayed task stages a referenced ordinal as a link to where
			// its producer wrote it, the bundle OUTS stages the skeleton of.
			target, err := filepath.EvalSymlinks(o)
			if err != nil {
				rp.t.Fatal(err)
			}

			if b := filepath.Dir(filepath.Dir(target)); !slices.Contains(bundles, b) && !slices.Contains(items, b) {
				bundles = append(bundles, b)
				lines.WriteString(rp.stageSkeleton(filepath.Join(refs, strconv.Itoa(len(bundles))), b))
			}
		}
	}

	return lines.String()
}

// stageSkeleton stages the skeleton of the bundle dir as a copy at at, as
// Nextflow stages OUTS's inputs, and returns its locations line.
func (rp *cgReplay) stageSkeleton(at, bundle string) string {
	rp.t.Helper()

	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		rp.t.Fatal(err)
	}

	if err := shim.CopyTree(filepath.Join(bundle, shim.SkeletonFile), at); err != nil {
		rp.t.Fatal(err)
	}

	return at + "\t" + bundle + "\n"
}

// publishManifest publishes the tree the manifest at path lists into
// results, as the workflow outputs do: each entry's source, where the task
// that wrote it keeps it or, local, beside the manifest, copied to its path.
func publishManifest(t *testing.T, path, results string) {
	t.Helper()

	var entries []struct {
		Path   string `json:"path"`
		Source string `json:"source"`
		Local  bool   `json:"local"`
	}

	readJSON(t, path, &entries)

	for _, e := range entries {
		src := e.Source
		if e.Local {
			src = filepath.Join(filepath.Dir(path), filepath.FromSlash(e.Source))
		}

		dst := filepath.Join(results, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := shim.CopyTree(src, dst); err != nil {
			t.Fatalf("publish %s: %v", e.Path, err)
		}
	}
}

// mre runs one call-graph subcommand against the fixture's invocation, from
// mrp's working dir, and returns its stderr.
func (rp *cgReplay) mre(argv ...string) (string, error) {
	cmd := exec.Command(filepath.Join(root, "mre"), slices.Concat([]string{argv[0], "-entry", rp.mro, "-mropath", rp.dir}, argv[1:])...)
	cmd.Dir = rp.tmp

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("mre %s: %w", strings.Join(argv, " "), err)
	}

	return stderr.String(), nil
}
