package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// The records of a split's chunks dir: each chunk's position and resources
// beside its args bundle, and the join's resources at the top.
const (
	chunkRecord    = "chunk.json"
	joinRecord     = "join.json"
	resourcesTable = "resources.tsv"
	joinRow        = "join"
	recordDirPerm  = 0o750
	// stageNameFile names the stage a job ran, in a file of mre's own beside
	// the job's metadata. _jobinfo's "name" carries only the top-level call,
	// and the Nextflow task graph is not a stable identity, so the mrp ABI
	// differential reads this to group invocations by stage. The adapter
	// never reads it, so it adds nothing to the _args/_jobinfo/_outs ABI.
	stageNameFile = "_mre_stage"
)

var (
	errPhase       = errors.New("-phase must be split, main or join")
	errNotSplit    = errors.New("the stage does not split")
	errNoChunk     = errors.New("no outs bundle for chunk")
	errChunkCount  = errors.New("chunks dir holds fewer chunks than the split returned")
	errNotChunkDir = errors.New("not a dir")
	errSpecialLine = errors.New("a special holding a line break has no resources table row")
)

// runFlags are the inputs of mre stage's job step beside the call graph's,
// and the dirs its task staged (staged), whose leaves the bundles it writes
// name by reference and the bundles it reads resolve their references
// among (refs).
type runFlags struct {
	phase, args, out, outRefs, chunk, chunks, chunkOuts, overrides string
	confine                                                        bool
	roots                                                          shim.Roots
	rt                                                             job.Runtime
	staged                                                         shim.Staged
	refs                                                           shim.Refs
}

func (f *runFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.phase, "phase", "", "the phase to run: split, main or join")
	fs.StringVar(&f.args, "args", "args", "the dir to link the files the instance's args name below")
	fs.StringVar(&f.out, "o", "", "split: the chunks dir to write; main of a split stage: the chunk's outs bundle dir")
	fs.StringVar(&f.outRefs, "out-refs", "", "split, and main of a split stage: file to write the references of the bundles -o receives into"+refsUsage)
	fs.StringVar(&f.chunk, "chunk", "", "main of a split stage: the chunk's dir in the split's chunks dir")
	fs.StringVar(&f.chunks, "chunks", "", "join: the split's chunks dir")
	fs.StringVar(&f.chunkOuts, "chunkouts", "", "join: the dir holding each chunk's outs bundle, named as its chunk")
	fs.StringVar(&f.rt.Adapters, "adapters", "", "Martian's adapters dir (default: the adapters mre embeds)")
	fs.StringVar(&f.rt.Mrjob, "mrjob", "", "the mrjob binary py and comp stages run under (default: the mrjob beside mre)")
	fs.BoolVar(&f.rt.Monitor, "monitor", false, "mrp --monitor: mrjob enforces the job's memory and thread limits")
	fs.StringVar(&f.rt.PSID, "psid", "", "the pipestance id job names carry")
	fs.StringVar(&f.rt.Work, "work", ".", "the dir to hold the job's metadata and journal")
	fs.BoolVar(&f.confine, "confine", false, "fail on a stage output that resolves outside the task work dir (isolated container targets) instead of copying it")
	fs.StringVar(&f.overrides, "overrides", "", "an mrp --overrides file over the job's resources (mre plan writes each node's own)")
	definePool(fs, &f.rt.Local)
}

// definePool defines the flags of the pool mrp's local job manager grants
// each job from, which without them is granted as mrp's cluster job
// managers grant it.
func definePool(fs *flag.FlagSet, p *job.Pool) {
	fs.IntVar(&p.Cores, "local-cores", 0, "mrp --localcores: grant each job from a local pool of this many cores, as mrp's local job manager does (default: as its cluster job managers do)")
	fs.IntVar(&p.MemGB, "local-mem-gb", 0, "mrp --localmem: the local pool's memory in GB")
}

// finished is what the last job of an instance hands its post: the outs
// it wrote, and how a bundle treats the files they name.
type finished struct {
	outs core.LazyArgumentMap
	opts shim.Opts
}

// run runs one phase of the stage instance inst, whose args are args, as
// mrp runs its job: a split writes the chunks dir, and a split stage's main
// its chunk's outs bundle; a non-split stage's main and a join return
// their job's outs.
func (f *runFlags) run(ctx context.Context, g *cg.Graph, inst cg.Instance, args core.LazyArgumentMap) (finished, error) {
	if err := f.rt.Local.Check(); err != nil {
		return finished{}, fmt.Errorf("run: %w", err)
	}

	over, err := readOverrides(f.overrides)
	if err != nil {
		return finished{}, fmt.Errorf("run: %w", err)
	}

	f.rt.Overrides = over

	in, err := f.instance(g, inst)
	if err != nil {
		return finished{}, err
	}

	if err := f.tools(in.Stage.Code); err != nil {
		return finished{}, err
	}

	switch f.phase {
	case job.PhaseSplit:
		return finished{}, f.split(ctx, g, in, args)
	case job.PhaseMain:
		return f.main(ctx, g, in, args)
	case job.PhaseJoin:
		return f.join(ctx, in, args)
	default:
		return finished{}, fmt.Errorf("%w: %q", errPhase, f.phase)
	}
}

// instance is the job instance of inst, and completes the runtime from the
// call graph.
func (f *runFlags) instance(g *cg.Graph, inst cg.Instance) (job.Instance, error) {
	st, err := g.Stage(inst.Node)
	if err != nil {
		return job.Instance{}, fmt.Errorf("run: %w", err)
	}

	stage, err := job.NewStage(st, g.MroPaths())
	if err != nil {
		return job.Instance{}, fmt.Errorf("run: %w", err)
	}

	if f.rt.Invocation, err = g.Invocation(); err != nil {
		return job.Instance{}, fmt.Errorf("run: %w", err)
	}

	f.rt.Version = job.Versions(g.MroPaths())

	forkID, err := g.ForkID(inst.Node, inst.Fork, inst.Lens)
	if err != nil {
		return job.Instance{}, fmt.Errorf("run: %w", err)
	}

	return job.Instance{Stage: stage, ForkID: forkID}, nil
}

func (f *runFlags) split(ctx context.Context, g *cg.Graph, in job.Instance, args core.LazyArgumentMap) error {
	if !in.Stage.Split {
		return fmt.Errorf("split of %s: %w", in.Stage.Fqid, errNotSplit)
	}

	params, err := g.Params(in.Stage.Callable, types.RoleChunkIn)
	if err != nil {
		return fmt.Errorf("split: %w", err)
	}

	if err := f.stamp(in, job.PhaseSplit); err != nil {
		return err
	}

	defs, err := f.rt.Split(ctx, in, args)
	if err != nil {
		return fmt.Errorf("split: %w", err)
	}

	w, err := f.writer(g, job.PhaseSplit)
	if err != nil {
		return err
	}

	refs, err := writeChunksDir(f.out, defs, params, w)
	if err != nil {
		return err
	}

	if err := writeResources(filepath.Join(f.out, resourcesTable), f.rt, in.Stage, defs); err != nil {
		return err
	}

	return writeRefs(f.outRefs, refs)
}

// readOverrides reads the mrp --overrides file at path, if any, with
// Martian's own reader, which refuses a key it does not know
// (core/override.go:96); none, nil, without one.
func readOverrides(path string) (*core.PipestanceOverrides, error) {
	if path == "" {
		return nil, nil
	}

	over, err := core.ReadOverrides(path)
	if err != nil {
		return nil, fmt.Errorf("read overrides %s: %w", path, err)
	}

	return over, nil
}

// writeResources writes the table a split's chunks and join are scheduled
// by: per chunk, then the join, its name, cpus, memory in GB and special,
// tab-separated, as each job's _jobinfo records them (Runtime.Resources),
// so the driver reads them without parsing the chunk records.
func writeResources(path string, rt job.Runtime, st job.Stage, defs *core.StageDefs) error {
	var b strings.Builder

	row := func(name, phase string, def *core.JobResources) error {
		res := rt.Resources(st, phase, def)
		if strings.ContainsAny(res.Special, "\n\r") {
			return fmt.Errorf("resources of %s: %w: %q", name, errSpecialLine, res.Special)
		}

		fmt.Fprintf(&b, "%s\t%d\t%s\t%s\n", name, job.CPUs(res), strconv.FormatFloat(res.MemGB, 'f', -1, 64), res.Special)

		return nil
	}

	for i, d := range defs.ChunkDefs {
		if err := row(job.ChunkName(i, len(defs.ChunkDefs)), job.PhaseMain, d.Resources); err != nil {
			return err
		}
	}

	if err := row(joinRow, job.PhaseJoin, defs.JoinDef); err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(b.String()), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// stamp names the instance's stage in stageNameFile in the job's metadata
// dir sub before the job runs, so a failed job is attributed too.
func (f *runFlags) stamp(in job.Instance, sub string) error {
	meta := filepath.Join(f.rt.Work, sub)
	if err := os.MkdirAll(meta, job.DirPerm); err != nil {
		return fmt.Errorf("stamp %s: %w", sub, err)
	}

	if err := os.WriteFile(filepath.Join(meta, stageNameFile), []byte(in.Stage.Callable), filePerm); err != nil {
		return fmt.Errorf("stamp %s: %w", sub, err)
	}

	return nil
}

// bundleOpts are how the bundle of what a job, whose metadata dir under the
// work dir is sub, returns treats the files it names: confined, strictly
// under -confine, but for those under the run's read-only roots.
func (f *runFlags) bundleOpts(sub string) (shim.Opts, error) {
	conf, err := shim.ConfineStage(f.rt.Work, sub, f.confine, f.roots)
	if err != nil {
		return shim.Opts{}, fmt.Errorf("confine %s outputs: %w", sub, err)
	}

	return shim.Opts{Confine: conf, Roots: f.roots}, nil
}

// writer writes the bundles a split stage's job, whose metadata dir under
// the work dir is sub, returns for a later task of the split to stage: the
// files they name treated as bundleOpts says, but each whole leaf of a
// bundle the task staged named by reference.
func (f *runFlags) writer(g *cg.Graph, sub string) (itemWriter, error) {
	opts, err := f.bundleOpts(sub)
	if err != nil {
		return itemWriter{}, err
	}

	return itemWriter{tbl: g.Table(), opts: opts, staged: f.staged}, nil
}

// itemWriter writes payloads as bundles another task stages, walking their
// file leaves through tbl: those its job wrote treated as opts says, each
// whole leaf of a bundle in staged named by reference (shim.WriteItem).
type itemWriter struct {
	tbl    *types.Table
	opts   shim.Opts
	staged shim.Staged
}

// write writes payload of params as the bundle dir and returns the records
// of the references it holds (shim.RefRecords).
func (w itemWriter) write(dir string, payload map[string]any, params []*syntax.StructMember) ([]string, error) {
	refs, err := shim.WriteItem(dir, payload, params, w.tbl, w.opts, w.staged)
	if err != nil {
		return nil, fmt.Errorf("write bundle %s: %w", dir, err)
	}

	return shim.RefRecords(dir, refs), nil
}

// writeRefs writes the reference records refs to the file path, which the
// driver resolves (-out-refs).
func writeRefs(path string, refs []string) error {
	if err := os.WriteFile(path, []byte(shim.JoinRefs(refs)), filePerm); err != nil {
		return fmt.Errorf("write -out-refs %q: %w", path, err)
	}

	return nil
}

// main runs the instance's main job: a split stage's chunk, whose outs it
// writes as the chunk's outs bundle, which the join stages, or a non-split
// stage's one chunk, whose outs it returns.
func (f *runFlags) main(ctx context.Context, g *cg.Graph, in job.Instance, args core.LazyArgumentMap) (finished, error) {
	chunk := job.Unsplit()

	var params []*syntax.StructMember

	if in.Stage.Split {
		c, err := readChunk(f.chunk, f.refs)
		if err != nil {
			return finished{}, err
		}

		if params, err = g.Params(in.Stage.Callable, types.RoleMainOut); err != nil {
			return finished{}, fmt.Errorf("main: %w", err)
		}

		chunk = c
	}

	sub := job.ChunkName(chunk.Index, chunk.Count)
	if err := f.stamp(in, sub); err != nil {
		return finished{}, err
	}

	outs, err := f.rt.Main(ctx, in, args, chunk)
	if err != nil {
		return finished{}, fmt.Errorf("main: %w", err)
	}

	if in.Stage.Split {
		return finished{}, f.writeChunkOuts(g, sub, outs, params)
	}

	opts, err := f.bundleOpts(sub)
	if err != nil {
		return finished{}, err
	}

	return finishedOf(outs, in.Stage.Outs, opts)
}

// writeChunkOuts writes the _outs of the chunk whose job's metadata dir is
// sub, raw, as its outs bundle, and the references it holds.
func (f *runFlags) writeChunkOuts(g *cg.Graph, sub string, raw json.RawMessage, params []*syntax.StructMember) error {
	w, err := f.writer(g, sub)
	if err != nil {
		return err
	}

	refs, err := writeOutsBundle(f.out, raw, params, w)
	if err != nil {
		return err
	}

	return writeRefs(f.outRefs, refs)
}

func (f *runFlags) join(ctx context.Context, in job.Instance, args core.LazyArgumentMap) (finished, error) {
	if !in.Stage.Split {
		return finished{}, fmt.Errorf("join of %s: %w", in.Stage.Fqid, errNotSplit)
	}

	defs, names, err := readChunksDir(f.chunks, f.refs)
	if err != nil {
		return finished{}, err
	}

	outs := make([]core.LazyArgumentMap, len(names))
	for i, name := range names {
		dir := filepath.Join(f.chunkOuts, name)
		if !shim.IsBundle(dir) {
			return finished{}, fmt.Errorf("join of %s: %w %s in %s", in.Stage.Fqid, errNoChunk, name, f.chunkOuts)
		}

		if outs[i], err = readArgsMap(dir, f.refs); err != nil {
			return finished{}, err
		}
	}

	if err := f.stamp(in, job.PhaseJoin); err != nil {
		return finished{}, err
	}

	final, err := f.rt.Join(ctx, in, args, defs, outs)
	if err != nil {
		return finished{}, fmt.Errorf("join: %w", err)
	}

	opts, err := f.bundleOpts(job.PhaseJoin)
	if err != nil {
		return finished{}, err
	}

	return finishedOf(final, in.Stage.Outs, opts)
}

// finishedOf is the instance's last job's _outs, raw, as its post takes
// them, with opts: each output's JSON compact, in its encoded key order
// and number lexemes, its file leaves where the job named them; none for a
// stage with no outputs (jobOuts).
func finishedOf(raw json.RawMessage, params *syntax.OutParams, opts shim.Opts) (finished, error) {
	payload, err := jobOuts(raw, params != nil && len(params.List) > 0)
	if err != nil {
		return finished{}, err
	}

	outs := make(core.LazyArgumentMap, len(payload))

	for k, v := range payload {
		enc, err := json.Marshal(v)
		if err != nil {
			return finished{}, fmt.Errorf("encode output %s: %w", k, err)
		}

		outs[k] = enc
	}

	return finished{outs: outs, opts: opts}, nil
}

// readArgsMap reads a bundle's payload object with its file leaves at their
// staged paths, those it names by reference among refs.
func readArgsMap(dir string, refs shim.Refs) (core.LazyArgumentMap, error) {
	raw, err := refs.ReadBundle(dir)
	if err != nil {
		return nil, fmt.Errorf("read bundle: %w", err)
	}

	var m core.LazyArgumentMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode bundle %s: %w", dir, err)
	}

	return m, nil
}

// chunkJSON is a chunk's record: its position among the split's chunks and
// the resources its def requests.
type chunkJSON struct {
	Index     int                `json:"index"`
	Count     int                `json:"count"`
	Resources *core.JobResources `json:"resources"`
}

// writeChunksDir writes each chunk def the split returned as a bundle of
// its args, the files the chunk params name staged or named by reference as
// w says, named by mrp's chunk name, and the join's resources beside them,
// and returns the records of the references. The split chose the files, so
// w's opts confine them.
func writeChunksDir(dir string, defs *core.StageDefs, params []*syntax.StructMember, w itemWriter) ([]string, error) {
	var refs []string

	for i, d := range defs.ChunkDefs {
		cd := filepath.Join(dir, job.ChunkName(i, len(defs.ChunkDefs)))

		named, err := writeObjectBundle(cd, d.Args, params, w)
		if err != nil {
			return nil, err
		}

		if err := writeRecordFile(filepath.Join(cd, chunkRecord), chunkJSON{Index: i, Count: len(defs.ChunkDefs), Resources: d.Resources}); err != nil {
			return nil, err
		}

		refs = append(refs, named...)
	}

	return refs, writeRecordFile(filepath.Join(dir, joinRecord), defs.JoinDef)
}

// readChunk reads a chunk the split wrote, the files it names by reference
// among refs.
func readChunk(dir string, refs shim.Refs) (job.Chunk, error) {
	var rec chunkJSON
	if err := readRecordFile(filepath.Join(dir, chunkRecord), &rec); err != nil {
		return job.Chunk{}, err
	}

	args, err := readArgsMap(dir, refs)
	if err != nil {
		return job.Chunk{}, err
	}

	return job.Chunk{Index: rec.Index, Count: rec.Count, Def: &core.ChunkDef{Resources: rec.Resources, Args: args}}, nil
}

// readChunksDir reads a split's chunks dir back into its stage defs, in
// chunk order, and the chunks' names, the files they name by reference
// among refs. It fails unless it reads every chunk the split returned, as
// each chunk's record counts them.
func readChunksDir(dir string, refs shim.Refs) (*core.StageDefs, []string, error) {
	defs := &core.StageDefs{}
	if err := readRecordFile(filepath.Join(dir, joinRecord), &defs.JoinDef); err != nil {
		return nil, nil, err
	}

	names, err := chunkNames(dir)
	if err != nil {
		return nil, nil, err
	}

	defs.ChunkDefs = make([]*core.ChunkDef, len(names))

	for i, name := range names {
		c, err := readChunk(filepath.Join(dir, name), refs)
		if err != nil {
			return nil, nil, err
		}

		if c.Count != len(names) {
			return nil, nil, fmt.Errorf("%w: %s is one of %d, %d staged", errChunkCount, name, c.Count, len(names))
		}

		defs.ChunkDefs[i] = c.Def
	}

	return defs, names, nil
}

// chunkNames are the chunk dirs in dir, sorted. A task stages each as a
// link to the split's, so each must resolve to a dir.
func chunkNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read chunks dir: %w", err)
	}

	var names []string

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "chnk") {
			continue
		}

		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("chunk %s: %w", e.Name(), err)
		}

		if !info.IsDir() {
			return nil, fmt.Errorf("chunk %s: %w", e.Name(), errNotChunkDir)
		}

		names = append(names, e.Name())
	}

	slices.Sort(names)

	return names, nil
}

// writeOutsBundle writes a job's _outs as a bundle of params, the files
// they name staged or named by reference as w says, and returns the records
// of the references.
func writeOutsBundle(dir string, raw json.RawMessage, params []*syntax.StructMember, w itemWriter) ([]string, error) {
	payload, err := jobOuts(raw, len(params) > 0)
	if err != nil {
		return nil, err
	}

	return w.write(dir, payload, params)
}

// jobOuts is a job's _outs, raw, by output, each value in its encoded key
// order and number lexemes, as mrp reads them (core/stage.go:1209-1214,
// :1280-1292): a stage with no outputs has none, whatever its job wrote,
// and one with outputs must have written an object, null, or the empty
// string the adapter writes for none.
func jobOuts(raw json.RawMessage, hasOuts bool) (map[string]any, error) {
	if !hasOuts {
		return map[string]any{}, nil
	}

	payload, err := rawToMap(raw)
	if err != nil {
		return nil, fmt.Errorf("_outs: %w", err)
	}

	return payload, nil
}

func writeObjectBundle(dir string, m core.LazyArgumentMap, params []*syntax.StructMember, w itemWriter) ([]string, error) {
	payload := make(map[string]any, len(m))

	for k, v := range m {
		dv, err := types.DecodeJSON(v)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", k, err)
		}

		payload[k] = dv
	}

	return w.write(dir, payload, params)
}

func writeRecordFile(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), recordDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, raw, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

func readRecordFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}
