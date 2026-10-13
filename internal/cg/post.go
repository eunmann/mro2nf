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

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
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
	// Outs is the stage's outs bundle.
	Outs string
	// Forks is the dir that receives <root>/<ordinal>/ fork elements.
	Forks string
	// Fields, when set, is the dir that receives one stamped bundle per
	// output, named by it, so a consumer stages only the outputs it reads.
	Fields string
	// Gates, when set, is the file that receives the nodes this instance's
	// outs disable at its fork (Gates).
	Gates string
	// Roots are the run's read-only roots, whose paths the elements and
	// fields name as they are.
	Roots shim.Roots
}

// Post stamps the instance's outs with its node and fork, checks them as mrp
// does, and writes one fork element dir per fork of every root this stage
// feeds and one partner element dir per element of each output a partner
// reads, then the outs field by field and the gates they set, when asked.
func (g *Graph) Post(spec PostSpec) error {
	in, s, err := g.post(spec)
	if err != nil {
		return err
	}

	if spec.Fields != "" {
		if err := g.writeFields(in, s, spec); err != nil {
			return err
		}
	}

	if spec.Gates != "" {
		return g.writeGates(in, s, spec.Gates)
	}

	return nil
}

func (g *Graph) post(spec PostSpec) (Instance, *stagedStore, error) {
	in, err := g.ReadInstance(spec.Args)
	if err != nil {
		return Instance{}, nil, err
	}

	st, err := g.Stage(in.Node)
	if err != nil {
		return Instance{}, nil, err
	}

	if err := stampPosted(spec.Outs, in); err != nil {
		return Instance{}, nil, err
	}

	s, err := g.stage([]string{spec.Outs})
	if err != nil {
		return Instance{}, nil, err
	}

	if err := g.verifyOutputs(st, s, in.Fork); err != nil {
		return Instance{}, nil, err
	}

	w := forkWriter{r: &resolver{g: g, st: s}, dir: spec.Forks, tbl: g.Table(), roots: spec.Roots, values: true}
	if err := w.posted(in); err != nil {
		return Instance{}, nil, err
	}

	return in, s, nil
}

// posted writes the forks of every root the posting instance's stage feeds,
// and its partner elements.
func (w forkWriter) posted(in Instance) error {
	for _, root := range w.r.g.sortedRoots() {
		if runtimeMultiSource(root) && slices.Contains(refIDs(root.Split().Value), in.Node) {
			return fmt.Errorf("%w: %s", errMultiSource, root.Fqid)
		}
	}

	for _, root := range w.r.g.fedBy(in.Node) {
		if err := w.root(root, counted{fork: in.Fork, lens: in.Lens}); err != nil {
			return err
		}
	}

	return w.writePartners(in.Node, in.Fork)
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

		if err := w.root(root, counted{}); err != nil {
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

// verifyOutputs checks a stage's outs as mrp's Fork.verifyOutput does
// (core/stage.go:764-786) with Martian's own LazyArgumentMap.ValidateOutputs:
// an error fails the stage, and an alarm is dropped, as at mrp's default
// enforcement level. mrp's --strict mode is not supported.
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
	}

	return nil
}

// forkWriter writes a root's forks as <dir>/<root>/<name>/, one dir per
// fork in mrp's order (core/fork.go:1639 expandForkFromObj). The name is the
// fork's ordinal, zero-padded so names sort in that order, after the
// ordinals of any outer parts the producer does not fix, joined by dots.
// With values, each dir also holds the fork's element of the source
// collection as a bundle, its file leaves staged but those under roots.
type forkWriter struct {
	r      *resolver
	dir    string
	tbl    *types.Table
	roots  shim.Roots
	values bool
}

// counted is a fork and the fork count of each of its parts.
type counted struct {
	fork Fork
	lens []int
}

func (c counted) with(p Part, n int) counted {
	return counted{fork: c.fork.with(p), lens: append(slices.Clip(c.lens), n)}
}

// root writes root's forks under the parts of fixed, the posting instance.
func (w forkWriter) root(root *syntax.CallGraphStage, fixed counted) error {
	dir := filepath.Join(w.dir, root.Fqid)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	return w.level(root, fixed, counted{}, w.r.g.outerRoots(root), nil, dir)
}

// level enumerates root's outer parts one root at a time: a part the
// posting instance's fork fixes, or every index of an outer root, from the
// call graph and what the task staged.
func (w forkWriter) level(root *syntax.CallGraphStage, fixed, prefix counted, rest syntax.ForkRootList, names []string, dir string) error {
	if len(rest) == 0 {
		return w.elements(root, prefix, names, dir)
	}

	o := rest[0]

	if i, ok := fixed.fork.rootPos(o); ok {
		return w.level(root, fixed, prefix.with(Part{Root: o, Index: fixed.fork[i].Index}, fixed.lens[i]), rest[1:], names, dir)
	}

	idx, err := w.r.sourceIndices(o, prefix.fork)
	if err != nil {
		return fmt.Errorf("forks of %s: %w", o.Fqid, err)
	}

	for i, ix := range idx {
		next := prefix.with(Part{Root: o, Index: ix}, len(idx))
		if err := w.level(root, fixed, next, rest[1:], append(slices.Clip(names), ordinal(i, len(idx))), dir); err != nil {
			return err
		}
	}

	return nil
}

func (w forkWriter) elements(root *syntax.CallGraphStage, outerAt counted, names []string, dir string) error {
	outer := outerAt.fork

	var (
		c   indexed
		idx []syntax.CollectionIndex
		err error
	)

	if w.values {
		c, err = w.source(root, outer)
		idx = c.indices()
	} else {
		idx, err = w.r.sourceIndices(root, outer)
	}

	if err != nil {
		return fmt.Errorf("forks of %s: %w", root.Fqid, err)
	}

	var param *syntax.StructMember

	if w.values && len(idx) > 0 {
		if param, err = w.r.g.collectionParam(elementField, root.Split(), root); err != nil {
			return err
		}
	}

	for i, ix := range idx {
		ed := filepath.Join(dir, elementName(names, i, len(idx), ix))
		at := outerAt.with(Part{Root: root, Index: ix}, len(idx))

		if w.values {
			if err := w.writeValue(ed, c, ix, param); err != nil {
				return fmt.Errorf("fork %d of %s: %w", i, root.Fqid, err)
			}
		}

		if err := writeElement(ed, Element{Fork: at.fork, Lens: at.lens, Position: i}); err != nil {
			return err
		}
	}

	return nil
}

// writeValue writes into ed the fork's element of the root's source c.
func (w forkWriter) writeValue(ed string, c indexed, ix syntax.CollectionIndex, param *syntax.StructMember) error {
	el, err := c.at(ix)
	if err != nil {
		return err
	}

	return writeElementValue(ed, el, param, w.tbl, w.roots)
}

// source is root's collection under outer, as core expands a fork from a
// stage's outs (core/fork.go:1576-1790).
func (w forkWriter) source(root *syntax.CallGraphStage, outer Fork) (indexed, error) {
	v, err := w.r.sourceValue(root, outer)
	if err != nil {
		return indexed{}, err
	}

	return indexedOf(v, root.Split().Source.CallMode())
}

// elementName names fork i of n at ix under the outer ordinals names. A map
// key's fork also carries its IndexToken after an @, which the driver
// matches a partner element by (partner); an array fork's is its ordinal.
func elementName(names []string, i, n int, ix syntax.CollectionIndex) string {
	name := strings.Join(append(slices.Clip(names), ordinal(i, n)), ".")
	if ix.Mode() == syntax.ModeMapCall {
		name += "@" + IndexToken(ix)
	}

	return name
}

func ordinal(i, n int) string {
	return fmt.Sprintf("%0*d", max(minOrdinalWidth, len(strconv.Itoa(n-1))), i)
}

// collectionParam is the param named name a fork element member of root is
// walked by for its file leaves: the element type of split's collection.
// Martian's ArrayType.ElementType is the innermost type whatever the
// dimension, so an element of a T[][] is a T[] only one dimension down.
func (g *Graph) collectionParam(name string, split *syntax.SplitExp, root *syntax.CallGraphStage) (*syntax.StructMember, error) {
	var t syntax.Type

	switch ct := split.Type.(type) {
	case *syntax.ArrayType:
		t = g.types.GetArray(ct.Elem, ct.Dim-1)
	case *syntax.TypedMapType:
		t = ct.Elem
	}

	if t == nil {
		return nil, fmt.Errorf("%w: split of %s has no collection type", errShape, root.Fqid)
	}

	return &syntax.StructMember{Id: name, Tname: t.TypeId()}, nil
}

// writeElementValue writes an element as a bundle of its one param.
func writeElementValue(dir string, el json.Marshaler, param *syntax.StructMember, tbl *types.Table, roots shim.Roots) error {
	raw, err := marshal(el)
	if err != nil {
		return err
	}

	v, err := types.DecodeJSON(raw)
	if err != nil {
		return fmt.Errorf("decode fork element: %w", err)
	}

	if err := shim.WriteBundle(dir, map[string]any{param.Id: v}, []*syntax.StructMember{param}, tbl, shim.Opts{Roots: roots}); err != nil {
		return fmt.Errorf("write fork element %s: %w", dir, err)
	}

	return nil
}
