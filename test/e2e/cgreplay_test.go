//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestMrpCallGraph runs each mrp-diff fixture under real mrp and replays its
// pipestance through the call-graph data plane: in DAG order, `mre args`
// builds every stage instance's args bundle from only what that instance
// stages (its fork elements and its parents' stamped outs), `mre run` runs
// each of its phases through mrjob as a stage task does, and `mre post`
// stamps its outs and writes the fork elements downstream instances fan out
// over; `mre outs` then resolves the pipeline outs. It requires:
//   - every instance's _args equal to mrp's after both are re-encoded and
//     file leaves are reduced to name and content digest, and its disabled
//     state to match mrp's _disabled marker;
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
			runMrpAt(t, bin, dir, mro, tmp)
			newCGReplay(t, tc.name, dir, mro, tmp, filepath.Join(filepath.Dir(bin), "adapters")).run()
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
	// adapters is mrp's adapters dir, which `mre run` runs py stages with.
	adapters string
	posted   map[string][]postedInstance
	ours     map[string]replayedFork
	// emptyFork holds the stages with a fork root that has no fork under
	// some fork of the roots before it.
	emptyFork map[string]bool
	seq       int
	jobs      int
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
// stages and the fork they give it.
type replayInstance struct {
	elems []string
	fork  cg.Fork
}

func newCGReplay(t *testing.T, fixture, dir, entry, tmp, adapters string) *cgReplay {
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
		adapters:  adapters,
		posted:    map[string][]postedInstance{},
		ours:      map[string]replayedFork{},
		emptyFork: map[string]bool{},
	}

	writeManifest(t, mro, dir, rp.types)

	return rp
}

func writeManifest(t *testing.T, mro, dir, path string) {
	t.Helper()

	_, _, ast, err := syntax.Compile(mro, []string{dir}, false)
	if err != nil {
		t.Fatal(err)
	}

	man := types.BuildManifest(frontend.Lower(ast))

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
	rp.t.Logf("call graph replay: %d instance _args equal mrp's, %d instances disabled, %d jobs run through mre run", ran, off, rp.jobs)
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

	rel := filepath.Join(strings.ReplaceAll(st.Fqid, ".", string(filepath.Separator)), inst.ForkID())
	mrpArgs, mrpErr := os.ReadFile(filepath.Join(rp.ps, rel, "split", argsFile))

	if disabled := state == "disabled"; disabled != rp.mrpDisabled(rel) {
		rp.t.Errorf("%s: mre args says %s, mrp's run disagrees", rel, state)
	}

	if state == "disabled" {
		rp.ours[rel] = replayedFork{disabled: true}
	} else {
		got := rp.checkArgs(rel, args, mrpArgs, mrpErr)
		rp.ours[rel] = replayedFork{args: got}
		rp.runStage(st, rel, args, outs, work)
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

// jobInfoStable are the _jobinfo keys whose values are the same every run;
// every other key (cwd, host, pid, rusage, ...) varies per run, so only its
// presence is compared.
var jobInfoStable = []string{
	"name", "type", "threads", "memGB", "vmemGB", "profile_mode",
	"stackvars_flag", "monitor_flag", "invocation", "version",
}

// runStage runs the instance's jobs with `mre run`, each in its own work
// dir as each runs in its own task, into the outs bundle outs, and checks
// them against the jobs mrp ran for the fork at rel: a stage that does not
// split runs one chunk, and a split stage its split, every chunk the split
// returns, and its join.
func (rp *cgReplay) runStage(st *syntax.CallGraphStage, rel, args, outs, work string) {
	if s, ok := st.Callable().(*syntax.Stage); !ok || !s.Split {
		for _, phase := range []string{job.PhaseSplit, job.PhaseJoin} {
			if regularFileExists(filepath.Join(rp.ps, rel, phase, "_jobinfo")) {
				rp.t.Errorf("%s: mrp ran a %s job for a stage that does not split", rel, phase)
			}
		}

		rp.runJob(rel, job.ChunkName(0, 1), work, job.PhaseMain, "-args", args, "-o", outs)

		return
	}

	chunks, chunkOuts := filepath.Join(work, "chunks"), filepath.Join(work, "chunkouts")
	rp.runJob(rel, job.PhaseSplit, work, job.PhaseSplit, "-args", args, "-o", chunks)

	ours, theirs := rp.chunkNames(chunks), rp.chunkNames(filepath.Join(rp.ps, rel))
	if !slices.Equal(ours, theirs) {
		rp.t.Errorf("%s: the split returned chunks %v, mrp's %v", rel, ours, theirs)
	}

	for _, c := range ours {
		rp.runJob(rel, c, work, job.PhaseMain, "-args", args, "-chunk", filepath.Join(chunks, c), "-o", filepath.Join(chunkOuts, c))
	}

	rp.runJob(rel, job.PhaseJoin, work, job.PhaseJoin, "-args", args, "-chunks", chunks, "-chunkouts", chunkOuts, "-o", outs)
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

// runJob runs one `mre run` job, whose metadata dir is name, in its own
// work dir, and checks its metadata against mrp's job of that name at rel.
// The work dir is laid out as a Nextflow task's, work/<2 hex>/<30 hex>, as
// the unpublished-file-paths allowlist rule recognizes it.
func (rp *cgReplay) runJob(rel, name, work, phase string, argv ...string) {
	rp.jobs++
	sum := sha256.Sum256([]byte(work + "\x00" + name))
	hash := hex.EncodeToString(sum[:])
	jw := filepath.Join(rp.tmp, "work", hash[:2], hash[2:32])
	rp.mre(append([]string{
		"run", "-phase", phase, "-psid", "mrp", "-adapters", rp.adapters,
		"-mrjob", filepath.Join(root, "mrjob"), "-work", jw,
	}, argv...)...)

	ours, theirs := filepath.Join(jw, name), filepath.Join(rp.ps, rel, name)
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
		rp.t.Errorf("%s: %s: mre run %v, mrp %v", job, f, oerr, merr)

		return
	}

	if g, w := rp.normJob(o, ours), rp.normJob(m, theirs); g != w {
		rp.t.Errorf("%s: %s differs from mrp's (-mrp +mre run):\n%s", job, f, cmp.Diff(w, g))
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
		rp.t.Errorf("%s: _jobinfo keys differ from mrp's (-mrp +mre run):\n%s", job, cmp.Diff(w, g))
	}

	for _, k := range jobInfoStable {
		if g, w := normJSON(rp.t, orNull(o[k]), leafNamed), normJSON(rp.t, orNull(m[k]), leafNamed); g != w {
			rp.t.Errorf("%s: _jobinfo %s differs from mrp's (-mrp +mre run):\n%s", job, k, cmp.Diff(w, g))
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
// and requires the tree and outs JSON mrp published, as TestMrpDiff compares
// them.
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
		"-leaves", filepath.Join(out, "f"), "-outs", "outs",
		"-types", rp.types, "-callable", rp.call.Call().DecId, "-role", types.RoleOut)

	// The OUTS task's publishDir copies the outs tree and these two files
	// into results/ (internal/cgnf/assets/stage.nf).
	results := filepath.Join(pub, "outs")
	if err := os.MkdirAll(results, 0o755); err != nil {
		rp.t.Fatal(err)
	}

	for _, f := range []string{"pipeline_outs.json", "manifest.json.gz"} {
		if err := os.Rename(filepath.Join(pub, f), filepath.Join(results, f)); err != nil {
			rp.t.Fatal(err)
		}
	}

	reports, err := diffPublished(mustAllowlist(rp.t), rp.fixture, rp.ps, results)
	if err != nil {
		rp.t.Fatal(err)
	}

	for _, r := range reports {
		rp.t.Error(r)
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
