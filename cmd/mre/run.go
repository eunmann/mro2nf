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
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
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
	errDisabledJob = errors.New("a disabled instance runs no job")
	errPhase       = errors.New("-phase must be split, main or join")
	errNotSplit    = errors.New("the stage does not split")
	errNoChunk     = errors.New("no outs bundle for chunk")
	errChunkCount  = errors.New("chunks dir holds fewer chunks than the split returned")
	errNotChunkDir = errors.New("not a dir")
	errSpecialLine = errors.New("a special holding a line break has no resources table row")
)

// runFlags are `mre run`'s inputs beside the call graph's.
type runFlags struct {
	phase, args, out, chunk, chunks, chunkOuts, overrides string
	confine                                               bool
	rt                                                    job.Runtime
}

func (f *runFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.phase, "phase", "", "the phase to run: split, main or join")
	fs.StringVar(&f.args, "args", "args", "the instance's args bundle dir")
	fs.StringVar(&f.out, "o", "", "split: the chunks dir to write; main and join: the outs bundle dir")
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
}

// runJob runs one phase of the stage instance whose args bundle -args
// holds, as mrp runs its job: split writes the chunks dir, main and join
// write the outs bundle.
func runJob(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	gf := addGraph(fs)

	var f runFlags

	f.register(fs)

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	return f.run(ctx, g, man)
}

func (f *runFlags) run(ctx context.Context, g *cg.Graph, man types.Manifest) error {
	over, err := readOverrides(f.overrides)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	f.rt.Overrides = over

	in, args, err := f.instance(g)
	if err != nil {
		return err
	}

	if err := f.tools(in.Stage.Code); err != nil {
		return err
	}

	switch f.phase {
	case job.PhaseSplit:
		return f.split(ctx, in, args, man)
	case job.PhaseMain:
		return f.main(ctx, in, args, man)
	case job.PhaseJoin:
		return f.join(ctx, in, args, man)
	default:
		return fmt.Errorf("%w: %q", errPhase, f.phase)
	}
}

// instance reads the instance the args bundle stamps, its stage and its
// bindings, and completes the runtime from the call graph.
func (f *runFlags) instance(g *cg.Graph) (job.Instance, core.LazyArgumentMap, error) {
	stamp, err := g.ReadInstance(f.args)
	if err != nil {
		return job.Instance{}, nil, fmt.Errorf("run: %w", err)
	}

	if stamp.Disabled {
		return job.Instance{}, nil, fmt.Errorf("run %s: %w", stamp.Node, errDisabledJob)
	}

	st, err := g.Stage(stamp.Node)
	if err != nil {
		return job.Instance{}, nil, fmt.Errorf("run: %w", err)
	}

	stage, err := job.NewStage(st, g.MroPaths())
	if err != nil {
		return job.Instance{}, nil, fmt.Errorf("run: %w", err)
	}

	if f.rt.Invocation, err = g.Invocation(); err != nil {
		return job.Instance{}, nil, fmt.Errorf("run: %w", err)
	}

	f.rt.Version = job.Versions(g.MroPaths())

	args, err := readArgsMap(f.args)
	if err != nil {
		return job.Instance{}, nil, err
	}

	return job.Instance{Stage: stage, ForkID: stamp.ForkID()}, args, nil
}

func (f *runFlags) split(ctx context.Context, in job.Instance, args core.LazyArgumentMap, man types.Manifest) error {
	if !in.Stage.Split {
		return fmt.Errorf("split of %s: %w", in.Stage.Fqid, errNotSplit)
	}

	if err := f.stamp(in, job.PhaseSplit); err != nil {
		return err
	}

	defs, err := f.rt.Split(ctx, in, args)
	if err != nil {
		return fmt.Errorf("split: %w", err)
	}

	conf, err := f.confinement(job.PhaseSplit)
	if err != nil {
		return err
	}

	if err := writeChunksDir(f.out, defs, man, callable(in), conf); err != nil {
		return err
	}

	return writeResources(filepath.Join(f.out, resourcesTable), f.rt, in.Stage, defs)
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

// confinement confines the files a job, whose metadata dir under the work
// dir is sub, names in what it returns; strict under -confine.
func (f *runFlags) confinement(sub string) (*shim.Confinement, error) {
	conf, err := shim.ConfineStage(f.rt.Work, sub, f.confine)
	if err != nil {
		return nil, fmt.Errorf("confine %s outputs: %w", sub, err)
	}

	return conf, nil
}

func (f *runFlags) main(ctx context.Context, in job.Instance, args core.LazyArgumentMap, man types.Manifest) error {
	chunk, role := job.Unsplit(), types.RoleOut

	if in.Stage.Split {
		c, err := readChunk(f.chunk)
		if err != nil {
			return err
		}

		chunk, role = c, types.RoleMainOut
	}

	if err := f.stamp(in, job.ChunkName(chunk.Index, chunk.Count)); err != nil {
		return err
	}

	outs, err := f.rt.Main(ctx, in, args, chunk)
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	conf, err := f.confinement(job.ChunkName(chunk.Index, chunk.Count))
	if err != nil {
		return err
	}

	return writeOutsBundle(f.out, outs, man, callable(in), role, conf)
}

func (f *runFlags) join(ctx context.Context, in job.Instance, args core.LazyArgumentMap, man types.Manifest) error {
	if !in.Stage.Split {
		return fmt.Errorf("join of %s: %w", in.Stage.Fqid, errNotSplit)
	}

	defs, names, err := readChunksDir(f.chunks)
	if err != nil {
		return err
	}

	outs := make([]core.LazyArgumentMap, len(names))
	for i, name := range names {
		dir := filepath.Join(f.chunkOuts, name)
		if !shim.IsBundle(dir) {
			return fmt.Errorf("join of %s: %w %s in %s", in.Stage.Fqid, errNoChunk, name, f.chunkOuts)
		}

		if outs[i], err = readArgsMap(dir); err != nil {
			return err
		}
	}

	if err := f.stamp(in, job.PhaseJoin); err != nil {
		return err
	}

	final, err := f.rt.Join(ctx, in, args, defs, outs)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}

	conf, err := f.confinement(job.PhaseJoin)
	if err != nil {
		return err
	}

	return writeOutsBundle(f.out, final, man, callable(in), types.RoleOut, conf)
}

// callable is the instance's stage name, its type manifest key.
func callable(in job.Instance) string { return in.Stage.Callable }

// readArgsMap reads a bundle's payload object with its file leaves at their
// staged paths.
func readArgsMap(dir string) (core.LazyArgumentMap, error) {
	raw, err := shim.ReadBundle(dir)
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
// its args, the files they name staged, named by mrp's chunk name, and the
// join's resources beside them. The split chose the files, so conf confines
// them.
func writeChunksDir(dir string, defs *core.StageDefs, man types.Manifest, callable string, conf *shim.Confinement) error {
	params, err := man.Params(callable, types.RoleChunkIn)
	if err != nil {
		return fmt.Errorf("chunk params of %s: %w", callable, err)
	}

	for i, d := range defs.ChunkDefs {
		cd := filepath.Join(dir, job.ChunkName(i, len(defs.ChunkDefs)))
		if err := writeObjectBundle(cd, d.Args, params, man.Table(), conf); err != nil {
			return err
		}

		if err := writeRecordFile(filepath.Join(cd, chunkRecord), chunkJSON{Index: i, Count: len(defs.ChunkDefs), Resources: d.Resources}); err != nil {
			return err
		}
	}

	return writeRecordFile(filepath.Join(dir, joinRecord), defs.JoinDef)
}

// readChunk reads a chunk the split wrote.
func readChunk(dir string) (job.Chunk, error) {
	var rec chunkJSON
	if err := readRecordFile(filepath.Join(dir, chunkRecord), &rec); err != nil {
		return job.Chunk{}, err
	}

	args, err := readArgsMap(dir)
	if err != nil {
		return job.Chunk{}, err
	}

	return job.Chunk{Index: rec.Index, Count: rec.Count, Def: &core.ChunkDef{Resources: rec.Resources, Args: args}}, nil
}

// readChunksDir reads a split's chunks dir back into its stage defs, in
// chunk order, and the chunks' names. It fails unless it reads every chunk
// the split returned, as each chunk's record counts them.
func readChunksDir(dir string) (*core.StageDefs, []string, error) {
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
		c, err := readChunk(filepath.Join(dir, name))
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

// writeOutsBundle writes a job's _outs as a bundle of callable's role
// params, the files they name staged and confined by conf. The adapter
// writes a stage with no outputs an _outs of "", which carries no outs.
func writeOutsBundle(dir string, raw json.RawMessage, man types.Manifest, callable, role string, conf *shim.Confinement) error {
	params, err := man.Params(callable, role)
	if err != nil {
		return fmt.Errorf("out params of %s: %w", callable, err)
	}

	v, err := types.DecodeJSON(raw)
	if err != nil {
		return fmt.Errorf("decode _outs: %w", err)
	}

	payload := map[string]any{}

	if obj, ok := types.AsObject(v); ok {
		for _, m := range obj {
			payload[m.Key] = m.Value
		}
	}

	return writeBundle(dir, payload, params, man.Table(), conf)
}

func writeObjectBundle(dir string, m core.LazyArgumentMap, params []ir.Param, tbl *types.Table, conf *shim.Confinement) error {
	payload := make(map[string]any, len(m))

	for k, v := range m {
		dv, err := types.DecodeJSON(v)
		if err != nil {
			return fmt.Errorf("decode %s: %w", k, err)
		}

		payload[k] = dv
	}

	return writeBundle(dir, payload, params, tbl, conf)
}

func writeBundle(dir string, payload map[string]any, params []ir.Param, tbl *types.Table, conf *shim.Confinement) error {
	if err := shim.WriteBundle(dir, payload, params, tbl, conf); err != nil {
		return fmt.Errorf("write bundle %s: %w", dir, err)
	}

	return nil
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
