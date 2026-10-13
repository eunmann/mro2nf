package cg

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// OutsSpec is the pipeline outs a task publishes.
type OutsSpec struct {
	// In are the staged outs of the stages the pipeline outs read, and
	// Values those it got as value records (Post's Values).
	In, Values []string
	// Locations maps each staged outs bundle, by its absolute path, to where
	// its producing task's work dir keeps it: the name of a file the
	// pipeline outs leave unpublished.
	Locations map[string]string
	// EntryFiles maps each entry file value the task staged to the path it
	// reads it at (StagedEntryFiles).
	EntryFiles map[string]string
	// Work is the dir the pipestance is reconstructed in.
	Work string
	// Results receives the published tree, and OutsJSON the outs JSON that
	// names each published file by its path in it.
	Results, OutsJSON string
}

// WriteOuts resolves the invocation's pipeline outs (core/resolve.go:183
// resolvePipelineOutputs), checks them as mrp does, and publishes them as
// mrp does, with Martian's own post-processing (core/post_process.go) on a
// pipestance reconstructed from them. Then it writes the published tree and
// the outs JSON naming it.
func (g *Graph) WriteOuts(ctx context.Context, spec OutsSpec) error {
	s, err := g.stage(spec.In, spec.Values)
	if err != nil {
		return err
	}

	v, err := g.pipelineOuts(s)
	if err != nil {
		return err
	}

	params, err := g.Params(g.root.Call().DecId, types.RoleOut)
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	ps, err := newPipestance(spec, s.outsItems(), g.Table(), params)
	if err != nil {
		return err
	}

	fork0 := filepath.Join(ps.dir, g.root.Call().Id, "fork0")
	if err := ps.reconstruct(v, filepath.Join(fork0, "_outs")); err != nil {
		return err
	}

	if err := g.postProcess(ctx, ps.dir); err != nil {
		return err
	}

	outs, err := ps.published(filepath.Join(fork0, "_outs"))
	if err != nil {
		return err
	}

	if err := ps.materialize(spec.Results); err != nil {
		return err
	}

	return writeJSONFile(spec.OutsJSON, outs)
}

// pipelineOuts resolves the invocation's pipeline outs from the staged
// stage outs and checks them.
func (g *Graph) pipelineOuts(s *stagedStore) (json.Marshaler, error) {
	r := &resolver{g: g, st: s}

	ro := g.root.ResolvedOutputs()
	if ro == nil || ro.Exp == nil {
		return core.MarshalerMap{}, nil
	}

	v, err := r.resolve(ro.Exp, ro.Type, nil)
	if err != nil {
		return nil, fmt.Errorf("pipeline outs: %w", err)
	}

	if ps := g.root.Callable().GetOutParams(); ps != nil && len(ps.List) > 0 {
		if err := g.verifyPipelineOutputs(v, ro.Type); err != nil {
			return nil, err
		}
	}

	return v, nil
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

// verifyPipelineOutputs checks resolved pipeline outs as mrp's
// Fork.verifyPipelineOutput does (core/stage.go:788-833), with Martian's own
// MarshalerMap.ValidatePipelineOutputs at each struct, dropping alarms as at
// mrp's default enforcement level.
func (g *Graph) verifyPipelineOutputs(v json.Marshaler, t syntax.Type) error {
	switch t := t.(type) {
	case *syntax.TypedMapType:
		if m, ok := v.(core.MarshalerMap); ok {
			for _, e := range m {
				if err := g.verifyPipelineOutputs(e, t.Elem); err != nil {
					return err
				}
			}
		}
	case *syntax.ArrayType:
		if a, ok := v.(marshalerArray); ok {
			for _, e := range a {
				if err := g.verifyPipelineOutputs(e, t.Elem); err != nil {
					return err
				}
			}
		}
	case *syntax.StructType:
		m, ok := v.(core.MarshalerMap)
		if !ok || len(t.Members) == 0 {
			return nil
		}

		if err, alarms := m.ValidatePipelineOutputs(g.types, t); err != nil {
			return fmt.Errorf("%w of the pipeline: %w%s", errInvalidOuts, err, alarms)
		}
	}

	return nil
}
