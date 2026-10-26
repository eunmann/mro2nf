package cg

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// OutsSpec is the pipeline outs a task publishes.
type OutsSpec struct {
	// In are the staged outs of the stages the pipeline outs read, and
	// Values those it got as value records (Post's Values).
	In, Values []string
	// Refs is the dir of the leaves the task staged by reference, which
	// the staged outs name by their origin where none of In holds them
	// (shim.Staged.Leaves).
	Refs string
	// Locations maps each staged outs bundle, and each bundle holding a
	// leaf staged by reference, by its absolute path, to where its
	// producing task's work dir keeps it: the name of a file the pipeline
	// outs leave unpublished.
	Locations map[string]string
	// EntryFiles maps each entry file value the task staged to the path it
	// reads it at (StagedEntryFiles).
	EntryFiles map[string]string
	// Work is the dir the pipestance is reconstructed in.
	Work string
	// Manifest receives the published tree as the run publishes it: a line
	// per entry, its path in the tree and, tab-separated, where the task
	// that wrote it keeps it. OutsJSON receives the outs JSON that names
	// each published file by its path in the tree.
	Manifest, OutsJSON string
}

// WriteOuts resolves the invocation's pipeline outs (core/resolve.go:184
// resolvePipelineOutputs), checks them as mrp does, and publishes them as
// mrp does, with Martian's own post-processing (core/post_process.go) on a
// pipestance reconstructed from them. Then it writes the manifest of the
// published tree and the outs JSON naming it. Post-processing reads no
// file's bytes, so the staged outs may be their skeletons
// (shim.ExpandSkeleton).
func (g *Graph) WriteOuts(ctx context.Context, spec OutsSpec) error {
	staged, err := shim.NewStaged(append(slices.Clone(spec.In), spec.Refs)...)
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	refs, err := staged.Leaves()
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	s, err := g.stage(spec.In, spec.Values, refs)
	if err != nil {
		return err
	}

	v, shape, err := g.pipelineOuts(s)
	if err != nil {
		return err
	}

	params, err := g.Params(g.root.Call().DecId, types.RoleOut)
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	ps, err := newPipestance(spec, s.outsItems(), forkShape{tbl: g.Table(), params: params, typ: shape})
	if err != nil {
		return err
	}

	fork0 := filepath.Join(ps.dir, g.root.Call().Id, "fork0")
	if err := ps.reconstruct(v, filepath.Join(fork0, "_outs")); err != nil {
		return err
	}

	if err := g.writeSourceGates(s, ps.dir); err != nil {
		return err
	}

	if err := g.postProcess(ctx, ps.dir); err != nil {
		return err
	}

	outs, err := ps.published(filepath.Join(fork0, "_outs"))
	if err != nil {
		return err
	}

	if err := ps.manifest(spec.Manifest); err != nil {
		return err
	}

	return writeJSONFile(spec.OutsJSON, outs)
}

// pipelineOuts resolves the invocation's pipeline outs from the staged
// stage outs and checks them, as mrp does (mrcore PipelineOutputs). It
// returns them with their type, an array or a map of the pipeline's outs
// for a top-level map call.
func (g *Graph) pipelineOuts(s *stagedStore) (json.Marshaler, syntax.Type, error) {
	ro := g.root.ResolvedOutputs()
	if ro == nil || ro.Exp == nil {
		return core.MarshalerMap{}, nil, nil
	}

	r := &resolver{g: g, st: s}

	v, t, err := r.core().PipelineOutputs(g.root.GetFqid(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("pipeline outs: %w", err)
	}

	return v, t, nil
}

// writeSourceGates writes into the pipestance at ps the outs of every
// instance of the stages sourceGates read, where an mrp pipestance holds
// them, at the instance's fork dir, so the reattach postProcess makes finds
// them ready.
func (g *Graph) writeSourceGates(s *stagedStore, ps string) error {
	r := &resolver{g: g, st: s}

	var ids []string
	for _, d := range g.sourceGates() {
		ids = union(ids, refIDs(d))
	}

	for _, id := range ids {
		st, err := g.Stage(id)
		if err != nil {
			return err
		}

		for _, f := range s.forks[id] {
			lens, err := r.forkLens(f)
			if err != nil {
				return fmt.Errorf("gate %s: %w", id, err)
			}

			outs, err := s.outs(st, f)
			if err != nil {
				return fmt.Errorf("gate %s: %w", id, err)
			}

			fid, err := g.ForkID(id, f, lens)
			if err != nil {
				return fmt.Errorf("gate %s: %w", id, err)
			}

			path := filepath.Join(ps, filepath.FromSlash(strings.ReplaceAll(id, ".", "/")), fid, "_outs")
			if err := writeJSONFile(path, outs); err != nil {
				return err
			}
		}
	}

	return nil
}

// forkLens is the fork count of each of f's roots under the parts of f
// its fork set varies with.
func (r *resolver) forkLens(f Fork) ([]int, error) {
	lens := make([]int, len(f))

	for i, p := range f {
		outer, _ := f.restrict(r.g.outerRoots(p.Root))

		idx, err := r.indices(p.Root, outer)
		if err != nil {
			return nil, err
		}

		lens[i] = len(idx)
	}

	return lens, nil
}

// postProcess runs Martian's PostProcess on the pipestance at ps, attached
// read-only from the invocation the graph compiled. Attaching needs no job
// manager, so the runtime is built without the one NewRuntime reads from
// Martian's install.
func (g *Graph) postProcess(ctx context.Context, ps string) error {
	opts := core.DefaultRuntimeOptions()
	opts.VdrMode = core.VdrDisable
	rt := &core.Runtime{Config: &opts}

	pipe, err := rt.ReattachToPipestance(filepath.Base(ps), ps, g.src, g.name, g.mroPaths, "", nil, false, true, ctx)
	if err != nil {
		return fmt.Errorf("post-process: attach %s: %w", ps, err)
	}

	pipe.PostProcess()

	return nil
}
