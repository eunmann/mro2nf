package job

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/eunmann/mro2nf/internal/mrcore"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
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

	outs := mrcore.OutArgs(in.Stage.Outs, j.files(), false)
	if in.Stage.Split {
		maps.Copy(outs, mrcore.OutArgs(in.Stage.ChunkOuts, j.files(), false))
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

	return r.runForOuts(ctx, in, j, mrcore.OutArgs(in.Stage.Outs, j.files(), false))
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
func (r Runtime) runForOuts(ctx context.Context, in Instance, j *job, outs core.MarshalerMap) (json.RawMessage, error) {
	if err := j.write(core.OutsFile, outs); err != nil {
		return nil, err
	}

	if err := r.run(ctx, in.Stage.Code, j); err != nil {
		return nil, err
	}

	raw, _, err := j.read(core.OutsFile)

	return raw, err
}
