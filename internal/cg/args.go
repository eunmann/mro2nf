package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Log receives the error of each input that read a missing element and
	// resolved to null, which mrp logs before it runs the fork. Nil discards
	// it.
	Log io.Writer
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

	fork, lens, err := instanceFork(st, s)
	if err != nil {
		return Instance{}, fmt.Errorf("fork of %s: %w", st.Fqid, err)
	}

	r := &resolver{g: g, st: s}

	in := Instance{Node: st.Fqid, Fork: fork, Lens: lens}
	if in.Disabled, err = r.disabled(st, fork); err != nil {
		return Instance{}, err
	}

	if !in.Disabled {
		args, unresolved, err := r.args(st, fork)
		if err != nil {
			return Instance{}, err
		}

		if err := reportUnresolved(spec.Log, in, unresolved); err != nil {
			return Instance{}, err
		}

		if err := writeValue(spec.Out, args, spec.Manifest, st.Call().DecId, types.RoleIn); err != nil {
			return Instance{}, err
		}
	}

	return in, writeInstance(spec.Out, in)
}

// reportUnresolved handles the inputs of in that read a missing element as
// mrp's getBindings does (core/stage.go:1402-1440): they fail the instance
// under the error enforcement level, and are logged otherwise.
func reportUnresolved(log io.Writer, in Instance, unresolved []error) error {
	if len(unresolved) == 0 {
		return nil
	}

	err := fmt.Errorf("resolve input bindings for %s at fork %s: %w", in.Node, in.Fork, errors.Join(unresolved...))
	if syntax.GetEnforcementLevel() == syntax.EnforceError {
		return err
	}

	if log != nil {
		if _, werr := fmt.Fprintln(log, err); werr != nil {
			return fmt.Errorf("log unresolved inputs: %w", werr)
		}
	}

	return nil
}

// instanceFork assembles the instance's fork, and each part's fork count,
// root by root from the parts the staged fork elements record, which must
// agree on one index per root.
func instanceFork(st *syntax.CallGraphStage, s *stagedStore) (Fork, []int, error) {
	fork := make(Fork, 0, len(st.ForkRoots()))
	lens := make([]int, 0, len(st.ForkRoots()))

	for _, root := range st.ForkRoots() {
		var (
			found []Part
			n     int
		)

		for _, el := range s.stagedElems {
			i, ok := el.Fork.rootPos(root)
			if !ok || !agrees(el.Fork, fork) {
				continue
			}

			if p := el.Fork[i]; !slices.ContainsFunc(found, func(q Part) bool { return indexEqual(q.Index, p.Index) }) {
				found, n = append(found, p), el.Lens[i]
			}
		}

		switch len(found) {
		case 0:
			return nil, nil, fmt.Errorf("%w %s", errNoForkPart, root.Fqid)
		case 1:
			fork, lens = append(fork, Part{Root: root, Index: found[0].Index}), append(lens, n)
		default:
			return nil, nil, fmt.Errorf("%w root %s: %d indices", errAmbiguous, root.Fqid, len(found))
		}
	}

	return fork, lens, nil
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
