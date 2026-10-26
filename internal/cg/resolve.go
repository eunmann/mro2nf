package cg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eunmann/mro2nf/internal/mrcore"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	// ErrNotStaged reports a value an instance's resolution reads that the
	// task did not stage: an upstream instance's outs or a fork element.
	ErrNotStaged = errors.New("upstream value not staged")

	errShape = errors.New("unexpected expression shape")

	// errNoElement is core's elementError for an index past the end of an
	// array or a key a map lacks (core/resolve.go:718 getArrayElement, :714
	// getMapElement). The element reads null, and an input that reads one
	// still runs under mrp's default enforcement level (core/stage.go:1402
	// getBindings), as a zipped split over a shorter source does.
	errNoElement = mrcore.ErrNoElement
)

// store serves the upstream values one task staged.
type store interface {
	// outs returns the outs stage st wrote at fork f (f has one part per
	// root of st), or an error wrapping ErrNotStaged.
	outs(st *syntax.CallGraphStage, f Fork) (core.LazyArgumentMap, error)
	// instances returns root's indices under outer as the staged instances
	// of the stages forked on root record them, in mrp's order.
	instances(root *syntax.CallGraphStage, outer Fork) []syntax.CollectionIndex
	// element returns the staged fork element of root at f, the fork of
	// root's outer roots plus root's own part: its element of root's own
	// source. It is false when no element is staged or it carries no value.
	element(root *syntax.CallGraphStage, f Fork) (json.RawMessage, bool, error)
	// partner returns the staged partner element of ref's output at index
	// ix of root (partner): its value, nil for a null output, or
	// errNoElement for an index it has no element at. It is false when no
	// partner element of the output is staged.
	partner(root *syntax.CallGraphStage, ref *syntax.RefExp, ix syntax.CollectionIndex) (json.RawMessage, bool, error)
}

// resolver evaluates call-graph expressions at a fork by Martian core's own
// resolution (mrcore) over the values one task staged.
type resolver struct {
	g   *Graph
	st  store
	top *mrcore.TopNode
}

// args resolves every input binding of st at fork into the map mrp writes as
// the fork's _args (core/resolve.go:61 resolveInputs). An input that reads a
// missing element keeps the value resolved around it, and its error is
// returned in unresolved, as resolveInputs keeps a binding's partial value
// beside its error.
func (r *resolver) args(st *syntax.CallGraphStage, fork Fork) (core.MarshalerMap, []error, error) {
	out, missing, err := r.core().Inputs(st.Fqid, coreFork(fork))
	if err != nil {
		return nil, nil, fmt.Errorf("args of %s: %w", st.Fqid, err)
	}

	if missing == nil {
		return out, nil, nil
	}

	return out, []error{missing}, nil
}

// disabled reports whether st's instance at fork, of the fork counts lens,
// is disabled, as core's Fork.disabled decides it (core/stage.go:883); a
// gate that does not resolve to a bool fails it, as it fails the stage
// under mrp.
func (r *resolver) disabled(st *syntax.CallGraphStage, fork Fork, lens []int) (bool, error) {
	cf, err := countedFork(fork, lens)
	if err != nil {
		return false, err
	}

	off, err := r.core().Disabled(st.Fqid, cf)
	if err != nil {
		return false, fmt.Errorf("instance of %s: %w", st.Fqid, err)
	}

	return off, nil
}

// disables reports whether gate, a disable binding or the gate of a
// disable-wrapped value, disables at fork (core/resolve.go:248
// resolveDisabledExp).
func (r *resolver) disables(gate syntax.Exp, fork Fork) (bool, error) {
	off, err := r.core().Disables(gate, coreFork(fork))
	if err != nil {
		return false, fmt.Errorf("disabled binding at fork %s: %w", fork, err)
	}

	return off, nil
}

// resolve evaluates exp of type t at fork (core/resolve.go:201).
func (r *resolver) resolve(exp syntax.Exp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	v, err := r.core().Resolve(exp, t, coreFork(fork))
	if err != nil {
		return v, fmt.Errorf("resolve at fork %s: %w", fork, err)
	}

	return v, nil
}

// specialize narrows exp to fork with Martian's BindingPath, as resolve
// does before it evaluates (core/resolve.go:210-227).
func (r *resolver) specialize(exp syntax.Exp, t syntax.Type, fork Fork) (syntax.Exp, syntax.Type, error) {
	if len(fork) == 0 {
		return exp, t, nil
	}

	rb := syntax.ResolvedBinding{Exp: exp, Type: t}

	b, err := rb.BindingPath("", fork.indexMap(), r.g.types)
	if err != nil {
		return nil, nil, fmt.Errorf("specialize binding at fork %s: %w", fork, err)
	}

	if b.Exp == exp {
		return exp, t, nil
	}

	return b.Exp, b.Type, nil
}

// resolveRef reads the referenced output of the upstream instance the
// reference matches at fork (core/resolve.go:883-933): projected and
// filtered to t by LazyArgumentMap.Path; a nil t keeps the declared type.
func (r *resolver) resolveRef(e *syntax.RefExp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	up, err := r.g.Stage(e.Id)
	if err != nil {
		return nil, fmt.Errorf("ref %s.%s: %w", e.Id, e.OutputId, err)
	}

	uf, err := fork.project(up.ForkRoots(), e.Forks)
	if err != nil {
		return nil, fmt.Errorf("ref %s.%s: %w", e.Id, e.OutputId, err)
	}

	outs, err := r.st.outs(up, uf)
	if err != nil {
		return nil, fmt.Errorf("ref %s.%s: %w", e.Id, e.OutputId, err)
	}

	src := r.g.types.Get(syntax.TypeId{Tname: up.Call().DecId})

	v, err := outs.Path(e.OutputId, src, t, r.g.types)
	if err != nil {
		return nil, fmt.Errorf("ref %s.%s: %w", e.Id, e.OutputId, err)
	}

	return v, nil
}

func marshal(v json.Marshaler) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}

	raw, err := v.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("encode value: %w", err)
	}

	return raw, nil
}

func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)

	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}
