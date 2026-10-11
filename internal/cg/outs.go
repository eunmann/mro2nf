package cg

import (
	"encoding/json"
	"fmt"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// OutsSpec is the pipeline outs a task resolves.
type OutsSpec struct {
	// In are the staged outs of the stages the pipeline outs read.
	In []string
	// Manifest locates the file leaves of the outs bundle.
	Manifest types.Manifest
	// Out is the outs bundle dir publish-layout reads.
	Out string
}

// WriteOuts resolves the invocation's pipeline outs (core/resolve.go:183
// resolvePipelineOutputs), checks them as mrp does, and writes them as a
// bundle of the top-level callable's outs.
func (g *Graph) WriteOuts(spec OutsSpec) error {
	s, err := g.stage(spec.In)
	if err != nil {
		return err
	}

	r := &resolver{g: g, st: s}

	var v json.Marshaler = core.MarshalerMap{}

	if ro := g.root.ResolvedOutputs(); ro != nil && ro.Exp != nil {
		if v, err = r.resolve(ro.Exp, ro.Type, nil); err != nil {
			return fmt.Errorf("pipeline outs: %w", err)
		}

		if ps := g.root.Callable().GetOutParams(); ps != nil && len(ps.List) > 0 {
			if err := g.verifyPipelineOutputs(v, ro.Type); err != nil {
				return err
			}
		}
	}

	return writeValue(spec.Out, v, spec.Manifest, g.root.Call().DecId, types.RoleOut)
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
