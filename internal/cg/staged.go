package cg

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// elementField is the one field of a fork element's bundle payload.
const elementField = "value"

// stagedStore serves the instance outs bundles, fork elements and partner
// elements one task staged, and those it got as value records, each
// identified by its own record, their leaves named by reference among the
// leaves it staged by reference, refs. An instance's outs may come as one
// bundle, or field by field (Post's Fields and Values) as bundles and value
// records, which merge.
type stagedStore struct {
	g           *Graph
	refs        shim.Refs
	outsDirs    map[string][]string
	outsValues  map[string]core.LazyArgumentMap
	forks       map[string][]Fork
	elements    map[string]elementItem
	partners    map[string]elementItem
	absent      map[string]*bool
	stagedElems []Element
	cache       map[string]core.LazyArgumentMap
}

// elementItem is where a fork or partner element's value is: the bundle dir
// the task staged, or the value its value record carried.
type elementItem struct {
	dir   string
	value json.RawMessage
}

// read is the element's value, its leaves named by reference among refs,
// and false for a dir that holds no bundle: a fork element of a root no
// stage feeds, whose collection is in the call graph.
func (it elementItem) read(refs shim.Refs) (json.RawMessage, bool, error) {
	if it.value != nil {
		return it.value, true, nil
	}

	if !shim.IsBundle(it.dir) {
		return nil, false, nil
	}

	raw, err := refs.ReadBundle(it.dir)
	if err != nil {
		return nil, false, fmt.Errorf("element %s: %w", it.dir, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, fmt.Errorf("decode element %s: %w", it.dir, err)
	}

	v, ok := payload[elementField]

	return v, ok, nil
}

func stagedKey(fqid string, f Fork) string { return fqid + "\x00" + f.key() }

// stage indexes the staged items under dirs, the value records values and
// the fork records recs, and the leaves staged by reference, refs. Each dir
// is an item (an instance bundle, a fork element or a partner element) or
// holds items as its subdirectories.
func (g *Graph) stage(dirs, values []string, refs shim.Refs, recs ...Element) (*stagedStore, error) {
	s := &stagedStore{
		g:          g,
		refs:       refs,
		outsDirs:   map[string][]string{},
		outsValues: map[string]core.LazyArgumentMap{},
		forks:      map[string][]Fork{},
		elements:   map[string]elementItem{},
		partners:   map[string]elementItem{},
		absent:     map[string]*bool{},
		cache:      map[string]core.LazyArgumentMap{},
	}

	for _, el := range recs {
		s.addElement(el, elementItem{})
	}

	for _, d := range dirs {
		if err := s.addTree(d); err != nil {
			return nil, err
		}
	}

	for _, v := range values {
		if err := s.addValue(v); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// addOuts indexes outs as the whole outs of the instance in.
func (s *stagedStore) addOuts(in Instance, outs core.LazyArgumentMap) {
	k := s.addInstance(in)
	s.outsValues[k] = maps.Clone(outs)

	if s.outsValues[k] == nil {
		s.outsValues[k] = core.LazyArgumentMap{}
	}
}

// addInstance records the instance whose outs the task staged a part of,
// and returns its key.
func (s *stagedStore) addInstance(in Instance) string {
	k := stagedKey(in.Node, in.Fork)

	_, dirs := s.outsDirs[k]
	if _, values := s.outsValues[k]; !dirs && !values {
		s.forks[in.Node] = append(s.forks[in.Node], in.Fork)
	}

	return k
}

func (s *stagedStore) addTree(dir string) error {
	if itemKind(dir) != "" {
		return s.add(dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("scan staged %s: %w", dir, err)
	}

	for _, e := range entries {
		if err := s.add(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}

	return nil
}

func (s *stagedStore) add(dir string) error {
	switch itemKind(dir) {
	case instanceFile:
		in, err := s.g.ReadInstance(dir)
		if err != nil {
			return err
		}

		k := s.addInstance(in)
		s.outsDirs[k] = append(s.outsDirs[k], dir)
	case elementFile:
		el, err := s.g.ReadElement(dir)
		if err != nil {
			return err
		}

		s.addElement(el, elementItem{dir: dir})
	case partnerFile:
		return s.addPartner(dir)
	default:
		return fmt.Errorf("staged %s: %w", dir, errNotItem)
	}

	return nil
}

// addElement indexes a fork element, where it holds its value, or none for
// a fork record that carries no value.
func (s *stagedStore) addElement(el Element, it elementItem) {
	root := el.Fork[len(el.Fork)-1].Root
	if it.dir != "" || it.value != nil {
		s.elements[stagedKey(root.Fqid, el.Fork)] = it
	}

	s.stagedElems = append(s.stagedElems, el)
}

func (s *stagedStore) outs(st *syntax.CallGraphStage, f Fork) (core.LazyArgumentMap, error) {
	k := stagedKey(st.Fqid, f)
	if m, ok := s.cache[k]; ok {
		return m, nil
	}

	dirs, ok := s.outsDirs[k]
	values, valued := s.outsValues[k]

	switch {
	case !ok && !valued && !hasOutputs(st):
		return core.LazyArgumentMap{}, nil
	case !ok && !valued:
		return nil, fmt.Errorf("outs of %s at fork %s: %w", st.Fqid, f, ErrNotStaged)
	}

	m := maps.Clone(values)
	if m == nil {
		m = core.LazyArgumentMap{}
	}

	for _, dir := range dirs {
		raw, err := s.refs.ReadBundle(dir)
		if err != nil {
			return nil, fmt.Errorf("staged outs of %s: %w", st.Fqid, err)
		}

		var part core.LazyArgumentMap
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, fmt.Errorf("decode staged outs of %s: %w", st.Fqid, err)
		}

		maps.Copy(m, part)
	}

	s.cache[k] = m

	return m, nil
}

// outsItems are the staged instance outs bundles.
func (s *stagedStore) outsItems() []string {
	var out []string

	for _, dirs := range s.outsDirs {
		out = append(out, dirs...)
	}

	slices.Sort(out)

	return out
}

// hasOutputs reports whether st declares outputs. A stage without any has
// the same empty outs at every instance, which no bundle carries when its
// outs are posted field by field.
func hasOutputs(st *syntax.CallGraphStage) bool {
	ps := st.Callable().GetOutParams()

	return ps != nil && len(ps.List) > 0
}

// instances collects root's indices from the forks of the staged instances
// of every stage forked on root that agree with outer.
func (s *stagedStore) instances(root *syntax.CallGraphStage, outer Fork) []syntax.CollectionIndex {
	seen := map[string]bool{}

	var out []syntax.CollectionIndex

	for _, forks := range s.forks {
		for _, f := range forks {
			p, ok := f.rootPart(root)
			if !ok || !agrees(f, outer) {
				continue
			}

			k := Fork{p}.key()
			if !seen[k] {
				seen[k] = true
				out = append(out, p.Index)
			}
		}
	}

	slices.SortFunc(out, compareIndex)

	return out
}

// agrees reports whether f and outer give every root they share the same
// index.
func agrees(f, outer Fork) bool {
	for _, o := range outer {
		if p, ok := f.rootPart(o.Root); ok && !indexEqual(p.Index, o.Index) {
			return false
		}
	}

	return true
}

// compareIndex orders indices as mrp orders forks: array position, or map
// keys in byte order.
func compareIndex(a, b syntax.CollectionIndex) int {
	if a.Mode() == syntax.ModeMapCall {
		switch {
		case a.MapKey() < b.MapKey():
			return -1
		case a.MapKey() > b.MapKey():
			return 1
		default:
			return 0
		}
	}

	return a.ArrayIndex() - b.ArrayIndex()
}

func (s *stagedStore) element(root *syntax.CallGraphStage, f Fork) (json.RawMessage, bool, error) {
	it, ok := s.elements[stagedKey(root.Fqid, f)]
	if !ok {
		return nil, false, nil
	}

	v, ok, err := it.read(s.refs)
	if err != nil {
		return nil, false, fmt.Errorf("fork element of %s: %w", root.Fqid, err)
	}

	return v, ok, nil
}

// emptyStore is a task with nothing staged: what the call graph alone fixes.
type emptyStore struct{}

func (emptyStore) outs(st *syntax.CallGraphStage, f Fork) (core.LazyArgumentMap, error) {
	return nil, fmt.Errorf("outs of %s at fork %s: %w", st.Fqid, f, ErrNotStaged)
}

func (emptyStore) instances(*syntax.CallGraphStage, Fork) []syntax.CollectionIndex { return nil }

func (emptyStore) element(*syntax.CallGraphStage, Fork) (json.RawMessage, bool, error) {
	return nil, false, nil
}
