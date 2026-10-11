package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoForkPart = errors.New("no staged fork element for fork root")
	errAmbiguous  = errors.New("staged fork elements disagree on")
)

// ArgsSpec is one stage instance whose _args a task builds.
type ArgsSpec struct {
	// Node is the stage's fqid.
	Node string
	// In are the staged items: instance outs bundles and fork elements. A
	// forked instance stages a fork element of each of its roots, or one
	// whose fork holds the parts of several.
	In []string
	// Manifest locates the file leaves the args bundle stages.
	Manifest types.Manifest
	// Out is the args bundle dir. A disabled instance gets only its stamp.
	Out string
}

// WriteArgs resolves the instance's disabled state and, when it runs, its
// _args into an args bundle, and stamps the bundle with the instance.
func (g *Graph) WriteArgs(spec ArgsSpec) (Instance, error) {
	st, err := g.Stage(spec.Node)
	if err != nil {
		return Instance{}, err
	}

	s, err := g.stage(spec.In)
	if err != nil {
		return Instance{}, err
	}

	fork, err := instanceFork(st, s)
	if err != nil {
		return Instance{}, fmt.Errorf("fork of %s: %w", st.Fqid, err)
	}

	r := &resolver{g: g, st: s}

	in := Instance{Node: st.Fqid, Fork: fork}
	if in.Disabled, err = r.disabled(st, fork); err != nil {
		return Instance{}, err
	}

	if !in.Disabled {
		args, err := r.args(st, fork)
		if err != nil {
			return Instance{}, err
		}

		if err := writeValue(spec.Out, args, spec.Manifest, st.Call().DecId, types.RoleIn); err != nil {
			return Instance{}, err
		}
	}

	return in, writeInstance(spec.Out, in)
}

// instanceFork assembles the instance's fork root by root from the parts
// the staged fork elements record, which must agree on one index per root.
func instanceFork(st *syntax.CallGraphStage, s *stagedStore) (Fork, error) {
	fork := make(Fork, 0, len(st.ForkRoots()))

	for _, root := range st.ForkRoots() {
		var found []Part

		for _, f := range s.elementForks {
			p, ok := f.rootPart(root)
			if ok && agrees(f, fork) && !slices.ContainsFunc(found, func(q Part) bool { return indexEqual(q.Index, p.Index) }) {
				found = append(found, p)
			}
		}

		switch len(found) {
		case 0:
			return nil, fmt.Errorf("%w %s", errNoForkPart, root.Fqid)
		case 1:
			fork = append(fork, Part{Root: root, Index: found[0].Index})
		default:
			return nil, fmt.Errorf("%w root %s: %d indices", errAmbiguous, root.Fqid, len(found))
		}
	}

	return fork, nil
}

// writeValue writes a resolved object as a bundle of callable's role params.
// Its top-level keys are written sorted, as a MarshalerMap encodes them, and
// nested values keep their encoded order and number lexemes.
func writeValue(dir string, v json.Marshaler, man types.Manifest, callable, role string) error {
	raw, err := marshal(v)
	if err != nil {
		return err
	}

	dv, err := types.DecodeJSON(raw)
	if err != nil {
		return fmt.Errorf("decode %s %s value: %w", callable, role, err)
	}

	payload := map[string]any{}
	if obj, ok := types.AsObject(dv); ok {
		for _, m := range obj {
			payload[m.Key] = m.Value
		}
	}

	params, err := man.Params(callable, role)
	if err != nil {
		return fmt.Errorf("params of %s: %w", callable, err)
	}

	if err := shim.WriteBundle(dir, payload, params, man.Table(), nil); err != nil {
		return fmt.Errorf("write %s bundle %s: %w", role, dir, err)
	}

	return nil
}
