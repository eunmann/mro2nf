package job

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"path"
	"strings"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// The defaults of jobmanagers/config.json's settings: an unset thread or
// memory request gets one thread and 1 GB, and an unset vmem request the
// memory plus 3 GB.
const (
	threadsPerJob = 1
	memGBPerJob   = 1
	extraVMemGB   = 3
	centiPerCore  = 100
	mbPerGB       = 1024
)

// Split runs the instance's split job (core/stage.go:1057 Fork.doSplit): its
// _args are the bindings alone, and it returns the stage defs the split
// wrote.
func (r Runtime) Split(ctx context.Context, in Instance, args core.LazyArgumentMap) (*core.StageDefs, error) {
	j, err := r.newJob(PhaseSplit, r.forkName(in), PhaseSplit)
	if err != nil {
		return nil, err
	}

	j.res = r.Resources(in.Stage, PhaseSplit, nil)

	if err := j.write(core.ArgsFile, args); err != nil {
		return nil, err
	}

	if err := r.run(ctx, in.Stage.Code, j); err != nil {
		return nil, err
	}

	raw, _, err := j.read(core.StageDefsFile)
	if err != nil {
		return nil, err
	}

	var defs core.StageDefs
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, fmt.Errorf("parse _stage_defs of %s: %w", j.name, err)
	}

	return &defs, nil
}

// Main runs chunk c of the instance (core/stage.go:284 Chunk.run): its
// resources resolved into the chunk def, its _args the bindings with the
// chunk def over them, and its _outs pre-populated. It returns the _outs the
// job wrote.
func (r Runtime) Main(ctx context.Context, in Instance, args core.LazyArgumentMap, c Chunk) (json.RawMessage, error) {
	chunk := ChunkName(c.Index, c.Count)

	j, err := r.newJob(PhaseMain, r.forkName(in)+"."+chunk, chunk)
	if err != nil {
		return nil, err
	}

	def := *c.Def
	res := r.Resources(in.Stage, PhaseMain, def.Resources)
	def.Resources = &res
	j.res = res

	if err := j.write(core.ArgsFile, def.MergeArguments(args)); err != nil {
		return nil, err
	}

	outs := outArgs(in.Stage.Outs, j.files())
	if in.Stage.Split {
		maps.Copy(outs, outArgs(in.Stage.ChunkOuts, j.files()))
	}

	return r.runForOuts(ctx, in, j, outs)
}

// Join runs the instance's join job (core/stage.go:1171 Fork.doJoin) over
// the split's defs and each chunk's outs, in chunk order.
func (r Runtime) Join(ctx context.Context, in Instance, args core.LazyArgumentMap,
	defs *core.StageDefs, chunkOuts []core.LazyArgumentMap,
) (json.RawMessage, error) {
	if len(chunkOuts) != len(defs.ChunkDefs) {
		return nil, fmt.Errorf("join of %s: %w: %d outs, %d chunks", in.Stage.Fqid, errChunks, len(chunkOuts), len(defs.ChunkDefs))
	}

	j, err := r.newJob(PhaseJoin, r.forkName(in), PhaseJoin)
	if err != nil {
		return nil, err
	}

	res := r.Resources(in.Stage, PhaseJoin, defs.JoinDef)
	j.res = res

	if err := j.write(core.ArgsFile, &core.ChunkDef{Resources: &res, Args: args}); err != nil {
		return nil, err
	}

	if err := writeChunks(j, in.Stage, defs, chunkOuts); err != nil {
		return nil, err
	}

	return r.runForOuts(ctx, in, j, outArgs(in.Stage.Outs, j.files()))
}

// writeChunks writes the join's _chunk_defs, each def's args without its
// resources, and _chunk_outs: the chunks' outs, or an empty object per
// chunk for a stage with no outputs (core/stage.go:1195-1246).
func writeChunks(j *job, st Stage, defs *core.StageDefs, chunkOuts []core.LazyArgumentMap) error {
	chunkDefs := make([]core.ChunkDef, len(defs.ChunkDefs))
	for i, d := range defs.ChunkDefs {
		chunkDefs[i].Args = d.Args
	}

	if err := j.write(core.ChunkDefsFile, chunkDefs); err != nil {
		return err
	}

	switch {
	case len(chunkOuts) == 0:
		return writeRaw(j, core.ChunkOutsFile, []byte("[]"))
	case hasParams(st.Outs) || hasParams(st.ChunkOuts):
		return j.write(core.ChunkOutsFile, chunkOuts)
	default:
		return writeRaw(j, core.ChunkOutsFile, []byte("["+strings.Repeat("{},", len(chunkOuts)-1)+"{}]"))
	}
}

func writeRaw(j *job, name core.MetadataFileName, raw []byte) error {
	if err := j.meta.WriteRawBytes(name, raw); err != nil {
		return fmt.Errorf("write _%s of %s: %w", name, j.name, err)
	}

	return nil
}

func hasParams(p *syntax.OutParams) bool { return p != nil && len(p.List) > 0 }

// runForOuts writes the job's pre-populated _outs, runs it, and returns the
// _outs it wrote.
func (r Runtime) runForOuts(ctx context.Context, in Instance, j *job, outs map[string]any) (json.RawMessage, error) {
	if err := j.write(core.OutsFile, outs); err != nil {
		return nil, err
	}

	if err := r.run(ctx, in.Stage.Code, j); err != nil {
		return nil, err
	}

	raw, _, err := j.read(core.OutsFile)

	return raw, err
}

// outArgs pre-populates _outs as core's makeOutArgs does (core/stage.go:44
// makeOutArg): an empty array or map for a collection, the default path in
// files for a file, and null otherwise.
func outArgs(params *syntax.OutParams, files string) map[string]any {
	out := map[string]any{}
	if params == nil {
		return out
	}

	for _, p := range params.List {
		switch m := &p.StructMember; {
		case m.Tname.ArrayDim > 0:
			out[p.Id] = []any{}
		case m.Tname.MapDim > 0:
			out[p.Id] = map[string]any{}
		case m.IsFile() == syntax.KindIsFile && m.GetOutFilename() != "":
			out[p.Id] = path.Join(files, m.GetOutFilename())
		default:
			out[p.Id] = nil
		}
	}

	return out
}

// Resources resolves the resources of st's job in phase as core's
// getJobReqs does (core/stage.go:1494): the stage's using() with the def's
// non-zero fields over it, then the run's overrides of the node's phase
// (core/override.go:159 ApplyResourceOverrides), resolved as the local job
// manager resolves a request (core/jobmanager_local.go:375 GetSystemReqs).
// A Nextflow task is sized to its request, so no host cap applies, and an
// adaptive negative request, which mrp's local manager grants the host's
// free capacity, resolves to its magnitude.
func (r Runtime) Resources(st Stage, phase string, def *core.JobResources) core.JobResources {
	res := request(st.Using, def)
	r.Overrides.ApplyResourceOverrides(r.fqname(st.Fqid), stageType(phase), &res)

	return granted(res)
}

// request is using with def's non-zero fields over it.
func request(using, def *core.JobResources) core.JobResources {
	var res core.JobResources
	if using != nil {
		res = *using
	}

	if def != nil {
		res.Threads = coalesce(def.Threads, res.Threads)
		res.MemGB = coalesce(def.MemGB, res.MemGB)
		res.VMemGB = coalesce(def.VMemGB, res.VMemGB)

		if def.Special != "" {
			res.Special = def.Special
		}
	}

	return res
}

// granted is res as the local job manager grants it, with no host cap.
func granted(res core.JobResources) core.JobResources {
	centi := math.Ceil(math.Abs(res.Threads) * centiPerCore)
	if centi == 0 {
		centi = threadsPerJob * centiPerCore
	}

	memMB := math.Ceil(math.Abs(res.MemGB) * mbPerGB)
	if memMB == 0 {
		memMB = memGBPerJob * mbPerGB
	}

	vmemMB := math.Ceil(math.Abs(res.VMemGB) * mbPerGB)
	if vmemMB == 0 {
		vmemMB = memMB + extraVMemGB*mbPerGB
	}

	res.Threads = centi / centiPerCore
	res.MemGB = memMB / mbPerGB
	res.VMemGB = max(vmemMB, memMB) / mbPerGB

	return res
}

func coalesce(a, b float64) float64 {
	if a != 0 {
		return a
	}

	return b
}
