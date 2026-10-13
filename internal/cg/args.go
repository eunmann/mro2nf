package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoForkPart = errors.New("no staged fork element for fork root")
	errAmbiguous  = errors.New("staged fork elements disagree on")
)

// ErrSpecDrift reports a task whose code is not the code its run was
// planned from: an image built from other sources, or a .mro edited since
// the project was transpiled.
var ErrSpecDrift = errors.New("the node compiles to another spec than the run's plan holds; re-transpile the pipeline, and rebuild its image, from the same sources")

// ArgsSpec is one stage instance whose _args a task builds.
type ArgsSpec struct {
	// Node is the stage's fqid.
	Node string
	// In are the staged items: instance outs bundles and fork elements. A
	// forked instance stages a fork element of each of its roots, or one
	// whose fork holds the parts of several.
	In []string
	// Values are the value records the task got as values rather than
	// staged: of outputs, fork elements and partner elements (Post's Values
	// and ForkValues).
	Values []string
	// Refs are the leaves the task staged, among which the references the
	// items it staged name resolve by origin (shim.Staged.Leaves).
	Refs shim.Refs
	// Forks are fork records the task got as values rather than staged:
	// those of the roots the invocation's entry values fix (DecodeElement).
	Forks []Element
	// Out is the dir the args' file leaves are linked below, which only its
	// own task reads: each links to the file the task staged
	// (shim.LinkLeaves), so a file every fork of a map call reads exists
	// once, its producer's.
	Out string
	// Log receives the error of each input that read a missing element and
	// resolved to null, which mrp logs before it runs the fork. Nil discards
	// it.
	Log io.Writer
	// Digest, when set, is the node's spec digest in the run's plan
	// (PlanNode.Digest), which the node must compile to.
	Digest string
	// EntryFiles maps each entry file value the task staged to the path it
	// reads it at (StagedEntryFiles).
	EntryFiles map[string]string
	// Roots are the run's read-only roots, whose paths the args name as they
	// are.
	Roots shim.Roots
}

// WriteArgs resolves the instance and, when it runs, its _args, each file
// leaf at its link below the spec's Out. A disabled instance has no args.
func (g *Graph) WriteArgs(spec ArgsSpec) (Instance, core.LazyArgumentMap, error) {
	st, err := g.Stage(spec.Node)
	if err != nil {
		return Instance{}, nil, err
	}

	if err := g.checkDigest(st, spec.Digest); err != nil {
		return Instance{}, nil, err
	}

	s, err := g.stage(spec.In, spec.Values, spec.Refs, spec.Forks...)
	if err != nil {
		return Instance{}, nil, err
	}

	fork, lens, err := instanceFork(st, s)
	if err != nil {
		return Instance{}, nil, fmt.Errorf("fork of %s: %w", st.Fqid, err)
	}

	r := &resolver{g: g, st: s}

	in := Instance{Node: st.Fqid, Fork: fork, Lens: lens}
	if in.Disabled, err = r.disabled(st, fork); err != nil || in.Disabled {
		return in, nil, err
	}

	args, unresolved, err := r.args(st, fork)
	if err != nil {
		return Instance{}, nil, err
	}

	if err := reportUnresolved(spec.Log, in, unresolved); err != nil {
		return Instance{}, nil, err
	}

	linked, err := g.linkArgs(spec, args, st.Call().DecId)
	if err != nil {
		return Instance{}, nil, err
	}

	return in, linked, nil
}

// checkDigest fails unless st compiles to the spec digest want, when set.
func (g *Graph) checkDigest(st *syntax.CallGraphStage, want string) error {
	if want == "" {
		return nil
	}

	got, err := g.specDigest(st)
	if err != nil {
		return err
	}

	if got != want {
		return fmt.Errorf("%s: %w", st.Fqid, ErrSpecDrift)
	}

	return nil
}

// reportUnresolved logs the inputs of in that read a missing element, as
// mrp's getBindings does at its default enforcement level
// (core/stage.go:1402-1440). mrp's --strict mode is not supported.
func reportUnresolved(log io.Writer, in Instance, unresolved []error) error {
	if len(unresolved) == 0 {
		return nil
	}

	err := fmt.Errorf("resolve input bindings for %s at fork %s: %w", in.Node, in.Fork, errors.Join(unresolved...))

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

// linkArgs links the file leaves of the instance's resolved args below the
// spec's Out and returns the args naming each at its link.
func (g *Graph) linkArgs(spec ArgsSpec, args json.Marshaler, callable string) (core.LazyArgumentMap, error) {
	payload, params, err := g.bundlePayload(args, callable, types.RoleIn, spec.EntryFiles)
	if err != nil {
		return nil, err
	}

	raw, err := shim.LinkLeaves(spec.Out, payload, params, g.Table(), spec.Roots)
	if err != nil {
		return nil, fmt.Errorf("link %s files below %s: %w", types.RoleIn, spec.Out, err)
	}

	var m core.LazyArgumentMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode %s of %s: %w", types.RoleIn, callable, err)
	}

	return m, nil
}

// bundlePayload is a resolved object as the payload of a bundle of
// callable's role params, and the params. Its top-level keys are written
// sorted, as a MarshalerMap encodes them, and nested values keep their
// encoded order and number lexemes. A file leaf staged names the path the
// bundle stages it from.
func (g *Graph) bundlePayload(v json.Marshaler, callable, role string, staged map[string]string) (map[string]any, []*syntax.StructMember, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, nil, err
	}

	payload, err := decodePayload(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s value: %w", callable, role, err)
	}

	params, err := g.Params(callable, role)
	if err != nil {
		return nil, nil, err
	}

	if len(staged) > 0 {
		payload, err = g.Table().Apply(params, payload, func(leaf string) (string, error) {
			if p, ok := staged[leaf]; ok {
				return p, nil
			}

			return leaf, nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("staged entry files of %s: %w", callable, err)
		}
	}

	return payload, params, nil
}
