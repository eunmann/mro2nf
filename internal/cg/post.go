package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// minOrdinalWidth is the least digit count of a fork element's ordinal name.
const minOrdinalWidth = 4

var (
	errInvalidOuts = errors.New("invalid stage outs")
	errMultiSource = errors.New("fork root reads several stages")
)

// PostSpec is one finished stage instance a task posts.
type PostSpec struct {
	// Args is the instance's args bundle dir, whose stamp names it.
	Args string
	// Outs is the stage's outs bundle; a disabled instance's is written here.
	Outs string
	// In are staged items a fork source of this stage also reads.
	In []string
	// Manifest locates the file leaves of the outs and fork elements.
	Manifest types.Manifest
	// Forks is the dir that receives <root>/<ordinal>/ fork elements.
	Forks string
}

// Post stamps the instance's outs with its node and fork, writing a disabled
// instance's outs as mrp does, checks enabled outs as mrp does, and writes
// one fork element dir per fork of every root this stage feeds.
func (g *Graph) Post(spec PostSpec) error {
	in, err := g.ReadInstance(spec.Args)
	if err != nil {
		return err
	}

	st, err := g.Stage(in.Node)
	if err != nil {
		return err
	}

	if in.Disabled {
		if err := writeValue(spec.Outs, disabledOuts(st), spec.Manifest, st.Call().DecId, types.RoleOut); err != nil {
			return err
		}
	}

	if err := writeInstance(spec.Outs, in); err != nil {
		return err
	}

	s, err := g.stage(append(slices.Clone(spec.In), spec.Outs))
	if err != nil {
		return err
	}

	if !in.Disabled {
		if err := g.verifyOutputs(st, s, in.Fork); err != nil {
			return err
		}
	}

	w := forkWriter{r: &resolver{g: g, st: s}, dir: spec.Forks, tbl: spec.Manifest.Table(), values: true}

	for _, root := range g.sortedRoots() {
		if runtimeMultiSource(root) && slices.Contains(refIDs(root.Split().Value), in.Node) {
			return fmt.Errorf("%w: %s", errMultiSource, root.Fqid)
		}

		if p, ok := g.Producer(root); ok && p == in.Node {
			if err := w.root(root, in.Fork); err != nil {
				return err
			}
		}
	}

	return nil
}

// PostEntry writes the fork records of every root no stage feeds, as the
// outs of the invocation's entry: the fork transport of a forked instance is
// a fork element whichever node fixes its forks. The elements carry no
// value, since such a split's collection is in the call graph.
func (g *Graph) PostEntry(forks string) error {
	w := forkWriter{r: &resolver{g: g, st: emptyStore{}}, dir: forks}

	for _, root := range g.sortedRoots() {
		if _, ok := g.Producer(root); ok || runtimeMultiSource(root) {
			continue
		}

		if err := w.root(root, nil); err != nil {
			return err
		}
	}

	return nil
}

func (g *Graph) sortedRoots() []*syntax.CallGraphStage {
	keys := slices.Sorted(maps.Keys(g.roots))
	out := make([]*syntax.CallGraphStage, len(keys))

	for i, k := range keys {
		out[i] = g.roots[k]
	}

	return out
}

// disabledOuts are a disabled fork's outs: every declared output null
// (core/stage.go:928 writeDisable, :57 makeOutArgs).
func disabledOuts(st *syntax.CallGraphStage) core.MarshalerMap {
	out := core.MarshalerMap{}

	if ps := st.Callable().GetOutParams(); ps != nil {
		for _, p := range ps.List {
			out[p.Id] = nil
		}
	}

	return out
}

// verifyOutputs checks a stage's outs as mrp's Fork.verifyOutput does
// (core/stage.go:764-786) with Martian's own LazyArgumentMap.ValidateOutputs:
// an error fails the stage, and an alarm only under the error enforcement
// level.
func (g *Graph) verifyOutputs(st *syntax.CallGraphStage, s *stagedStore, fork Fork) error {
	params := st.Callable().GetOutParams()
	if params == nil || len(params.List) == 0 {
		return nil
	}

	outs, err := s.outs(st, fork)
	if err != nil {
		return err
	}

	if err, alarms := outs.ValidateOutputs(g.types, params); err != nil {
		return fmt.Errorf("%w of %s: %w%s", errInvalidOuts, st.Fqid, err, alarms)
	} else if alarms != "" && syntax.GetEnforcementLevel() == syntax.EnforceError {
		return fmt.Errorf("%w of %s: %s", errInvalidOuts, st.Fqid, alarms)
	}

	return nil
}

// forkWriter writes a root's forks as <dir>/<root>/<name>/, one dir per
// fork in mrp's order (core/fork.go:1639 expandForkFromObj). The name is the
// fork's ordinal, zero-padded so names sort in that order, after the
// ordinals of any outer parts the producer does not fix, joined by dots.
// With values, each dir also holds the fork's element of the source
// collection as a bundle, its file leaves staged.
type forkWriter struct {
	r      *resolver
	dir    string
	tbl    *types.Table
	values bool
}

func (w forkWriter) root(root *syntax.CallGraphStage, fixed Fork) error {
	dir := filepath.Join(w.dir, root.Fqid)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	return w.level(root, fixed, nil, w.r.g.outerRoots(root), nil, dir)
}

// level enumerates root's outer parts one root at a time: a part the
// posting instance's fork fixes, or every index of an outer root, from the
// call graph and what the task staged.
func (w forkWriter) level(root *syntax.CallGraphStage, fixed, prefix Fork, rest syntax.ForkRootList, names []string, dir string) error {
	if len(rest) == 0 {
		return w.elements(root, prefix, names, dir)
	}

	o := rest[0]

	if p, ok := fixed.rootPart(o); ok {
		return w.level(root, fixed, prefix.with(Part{Root: o, Index: p.Index}), rest[1:], names, dir)
	}

	idx, err := w.r.sourceIndices(o, prefix)
	if err != nil {
		return fmt.Errorf("forks of %s: %w", o.Fqid, err)
	}

	for i, ix := range idx {
		if err := w.level(root, fixed, prefix.with(Part{Root: o, Index: ix}), rest[1:], append(slices.Clip(names), ordinal(i, len(idx))), dir); err != nil {
			return err
		}
	}

	return nil
}

func (w forkWriter) elements(root *syntax.CallGraphStage, outer Fork, names []string, dir string) error {
	var (
		v   json.Marshaler
		idx []syntax.CollectionIndex
		err error
	)

	if w.values {
		v, idx, err = w.source(root, outer)
	} else {
		idx, err = w.r.sourceIndices(root, outer)
	}

	if err != nil {
		return fmt.Errorf("forks of %s: %w", root.Fqid, err)
	}

	var param ir.Param

	if w.values && len(idx) > 0 {
		if param, err = elementParam(root); err != nil {
			return err
		}
	}

	for i, ix := range idx {
		ed := filepath.Join(dir, strings.Join(append(slices.Clip(names), ordinal(i, len(idx))), "."))

		if w.values {
			el, err := element(v, ix)
			if err != nil {
				return fmt.Errorf("fork %d of %s: %w", i, root.Fqid, err)
			}

			if err := writeElementValue(ed, el, param, w.tbl); err != nil {
				return err
			}
		}

		if err := writeElement(ed, Element{Fork: outer.with(Part{Root: root, Index: ix}), Position: i}); err != nil {
			return err
		}
	}

	return nil
}

// source is root's collection under outer and its fork indices, as core
// expands a fork from a stage's outs (core/fork.go:1576-1790).
func (w forkWriter) source(root *syntax.CallGraphStage, outer Fork) (json.Marshaler, []syntax.CollectionIndex, error) {
	v, err := w.r.sourceValue(root, outer)
	if err != nil {
		return nil, nil, err
	}

	raw, err := marshal(v)
	if err != nil {
		return nil, nil, err
	}

	idx, err := indicesOf(raw, root.Split().Source.CallMode())

	return v, idx, err
}

func ordinal(i, n int) string {
	return fmt.Sprintf("%0*d", max(minOrdinalWidth, len(strconv.Itoa(n-1))), i)
}

// elementParam is the param a fork element of root is walked by for its
// file leaves: the element type of root's split collection.
func elementParam(root *syntax.CallGraphStage) (ir.Param, error) {
	var t syntax.Type
	if ct := root.Split().Type; ct != nil {
		t = ct.ElementType()
	}

	if t == nil {
		return ir.Param{}, fmt.Errorf("%w: split of %s has no collection type", errShape, root.Fqid)
	}

	id := t.TypeId()
	fk := t.IsFile()

	return ir.Param{
		Name:     elementField,
		Type:     t.String(),
		BaseType: id.Tname,
		ArrayDim: int(id.ArrayDim),
		MapDim:   int(id.MapDim),
		IsFile:   fk == syntax.KindIsFile || fk == syntax.KindIsDirectory,
	}, nil
}

func writeElementValue(dir string, el json.Marshaler, p ir.Param, tbl *types.Table) error {
	raw, err := marshal(el)
	if err != nil {
		return err
	}

	v, err := types.DecodeJSON(raw)
	if err != nil {
		return fmt.Errorf("decode fork element: %w", err)
	}

	if err := shim.WriteBundle(dir, map[string]any{elementField: v}, []ir.Param{p}, tbl, nil); err != nil {
		return fmt.Errorf("write fork element %s: %w", dir, err)
	}

	return nil
}
