package cg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	// ErrNotStaged reports a value an instance's resolution reads that the
	// task did not stage: an upstream instance's outs or a fork element.
	ErrNotStaged = errors.New("upstream value not staged")

	errShape    = errors.New("unexpected expression shape")
	errNullGate = errors.New("disabled is bound to a null value")
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
	// root's outer roots plus root's own part.
	element(root *syntax.CallGraphStage, f Fork) (json.RawMessage, bool, error)
}

// resolver evaluates call-graph expressions at a fork as core TopNode.resolve
// does (core/resolve.go:200), over Martian's exported pieces: BindingPath
// specializes a binding for the fork, LazyArgumentMap.Path projects and
// type-filters an upstream's outs.
type resolver struct {
	g  *Graph
	st store
}

func (r *resolver) boolType() syntax.Type {
	return r.g.types.Get(syntax.TypeId{Tname: syntax.KindBool})
}

// args resolves every input binding of st at fork into the map mrp writes as
// the fork's _args (core/resolve.go:60 resolveInputs).
func (r *resolver) args(st *syntax.CallGraphStage, fork Fork) (core.MarshalerMap, error) {
	out := make(core.MarshalerMap, len(st.ResolvedInputs()))

	for k, rb := range st.ResolvedInputs() {
		v, err := r.resolve(rb.Exp, rb.Type, fork)
		if err != nil {
			return nil, fmt.Errorf("resolve %s input %s: %w", st.Fqid, k, err)
		}

		out[k] = v
	}

	return out, nil
}

// disabled evaluates st's disable bindings at fork as core Fork.disabled
// does (core/stage.go:883): the first true disables it, and a gate that
// resolves to no value is an error, which fails the stage under mrp.
func (r *resolver) disabled(st *syntax.CallGraphStage, fork Fork) (bool, error) {
	var errs []error

	for _, d := range st.Disabled() {
		v, err := r.resolve(d, r.boolType(), fork)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		on, err := gateValue(v)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		if on {
			return true, nil
		}
	}

	if err := errors.Join(errs...); err != nil {
		return false, fmt.Errorf("disabled state of %s: %w", st.Fqid, err)
	}

	return false, nil
}

// gateValue reads a resolved disable binding (core/stage.go:898-922): a JSON
// null from a stage output reads as false, as json.Unmarshal into a bool
// leaves it unset.
func gateValue(v json.Marshaler) (bool, error) {
	switch d := v.(type) {
	case nil, *syntax.NullExp:
		return false, errNullGate
	case *syntax.BoolExp:
		return d.Value, nil
	case json.RawMessage:
		var b bool
		if err := json.Unmarshal(d, &b); err != nil {
			return false, fmt.Errorf("decode disable gate %s: %w", d, err)
		}

		return b, nil
	default:
		return false, fmt.Errorf("%w: disable gate of type %T", errShape, v)
	}
}

// resolve evaluates exp of type t at fork (core/resolve.go:200).
func (r *resolver) resolve(exp syntax.Exp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	if exp == nil {
		return nil, nil
	}

	if !exp.HasRef() && !exp.HasSplit() {
		return exp, nil
	}

	exp, t, err := r.specialize(exp, t, fork)
	if err != nil || (!exp.HasRef() && !exp.HasSplit()) {
		return exp, err
	}

	switch e := exp.(type) {
	case *syntax.RefExp:
		return r.resolveRef(e, t, fork)
	case *syntax.ArrayExp:
		return r.resolveArray(e, t, fork)
	case *syntax.MapExp:
		return r.resolveMap(e, t, fork)
	case *syntax.SplitExp:
		return r.resolveSplit(e, t, fork)
	case *syntax.MergeExp:
		return r.resolveMerge(e, t, fork)
	case *syntax.DisabledExp:
		return r.resolveDisabled(e, t, fork)
	default:
		return nil, fmt.Errorf("%w: %T", errShape, exp)
	}
}

// specialize narrows exp to fork with Martian's BindingPath, as resolve
// does before it evaluates (core/resolve.go:209-226).
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
// reference matches at fork (core/resolve.go:832-882): projected and
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

// resolveArray resolves an array literal element-wise (core/resolve.go:1280).
func (r *resolver) resolveArray(e *syntax.ArrayExp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	at, ok := t.(*syntax.ArrayType)
	if !ok {
		return nil, fmt.Errorf("%w: array literal of type %s", errShape, t)
	}

	et := r.g.types.GetArray(at.Elem, at.Dim-1)
	out := make(marshalerArray, len(e.Value))

	for i, v := range e.Value {
		rv, err := r.resolve(v, et, fork)
		if err != nil {
			return nil, fmt.Errorf("array index %d: %w", i, err)
		}

		out[i] = rv
	}

	return out, nil
}

// resolveMap resolves a typed-map or struct literal member-wise into a
// MarshalerMap, whose keys marshal sorted (core/resolve.go:1311).
func (r *resolver) resolveMap(e *syntax.MapExp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	out := make(core.MarshalerMap, len(e.Value))

	switch mt := t.(type) {
	case *syntax.TypedMapType:
		for k, v := range e.Value {
			rv, err := r.resolve(v, mt.Elem, fork)
			if err != nil {
				return nil, fmt.Errorf("key %s: %w", k, err)
			}

			out[k] = rv
		}
	case *syntax.StructType:
		for _, m := range mt.Members {
			rv, err := r.resolve(e.Value[m.Id], r.g.types.Get(m.Tname), fork)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", m.Id, err)
			}

			out[m.Id] = rv
		}
	default:
		return nil, fmt.Errorf("%w: map literal of type %s", errShape, t)
	}

	return out, nil
}

// resolveDisabled resolves a disable-wrapped value: null when its gate is
// true (core/resolve.go:247-290).
func (r *resolver) resolveDisabled(e *syntax.DisabledExp, t syntax.Type, fork Fork) (json.Marshaler, error) {
	d, err := r.resolve(e.Disabled, r.boolType(), fork)
	if err != nil {
		return nil, fmt.Errorf("disabled binding: %w", err)
	}

	switch g := d.(type) {
	case *syntax.BoolExp:
		if g.Value {
			return nil, nil
		}
	case json.RawMessage:
		var on bool
		if err := json.Unmarshal(g, &on); err != nil {
			return nil, fmt.Errorf("disabled binding: %w", err)
		}

		if on {
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("%w: disabled binding of type %T from %s", errShape, d, e.Disabled.GoString())
	}

	v, err := r.resolve(e.Value, t, fork)
	if err != nil {
		return nil, fmt.Errorf("enabled value: %w", err)
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

// marshalerArray is core's marshallerArray (core/resolve.go:1155): a nil
// array encodes as null, and a nil element as null.
type marshalerArray []json.Marshaler

func (a marshalerArray) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("null"), nil
	}

	var buf bytes.Buffer

	buf.WriteByte('[')

	for i, v := range a {
		if i > 0 {
			buf.WriteByte(',')
		}

		raw, err := marshal(v)
		if err != nil {
			return nil, err
		}

		buf.Write(raw)
	}

	buf.WriteByte(']')

	return buf.Bytes(), nil
}
