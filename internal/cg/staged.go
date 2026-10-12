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

// stagedStore serves the instance outs bundles and fork elements one task
// staged, each identified by its own record. An instance's outs may come as
// one bundle or as one bundle per field (Post's Fields), which merge.
type stagedStore struct {
	g           *Graph
	outsDirs    map[string][]string
	forks       map[string][]Fork
	elements    map[string]string
	missing     map[string][]string
	stagedElems []Element
	cache       map[string]core.LazyArgumentMap
}

func stagedKey(fqid string, f Fork) string { return fqid + "\x00" + f.key() }

// stage indexes the staged items under dirs and the fork records recs. Each
// dir is an item (an instance bundle or a fork element) or holds items as
// its subdirectories.
func (g *Graph) stage(dirs []string, recs ...Element) (*stagedStore, error) {
	s := &stagedStore{
		g:        g,
		outsDirs: map[string][]string{},
		forks:    map[string][]Fork{},
		elements: map[string]string{},
		missing:  map[string][]string{},
		cache:    map[string]core.LazyArgumentMap{},
	}

	for _, el := range recs {
		s.addElement(el, "")
	}

	for _, d := range dirs {
		if err := s.addTree(d); err != nil {
			return nil, err
		}
	}

	return s, nil
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

		k := stagedKey(in.Node, in.Fork)
		if _, seen := s.outsDirs[k]; !seen {
			s.forks[in.Node] = append(s.forks[in.Node], in.Fork)
		}

		s.outsDirs[k] = append(s.outsDirs[k], dir)
	case elementFile:
		el, err := s.g.ReadElement(dir)
		if err != nil {
			return err
		}

		s.addElement(el, dir)
	default:
		return fmt.Errorf("staged %s: %w", dir, errNotItem)
	}

	return nil
}

// addElement indexes a fork element; dir is its bundle, or "" for a fork
// record that carries no value.
func (s *stagedStore) addElement(el Element, dir string) {
	root := el.Fork[len(el.Fork)-1].Root
	if dir != "" {
		s.elements[stagedKey(root.Fqid, el.Fork)] = dir
		s.missing[stagedKey(root.Fqid, el.Fork)] = el.Missing
	}

	s.stagedElems = append(s.stagedElems, el)
}

func (s *stagedStore) outs(st *syntax.CallGraphStage, f Fork) (core.LazyArgumentMap, error) {
	k := stagedKey(st.Fqid, f)
	if m, ok := s.cache[k]; ok {
		return m, nil
	}

	dirs, ok := s.outsDirs[k]

	switch {
	case !ok && !hasOutputs(st):
		return core.LazyArgumentMap{}, nil
	case !ok:
		return nil, fmt.Errorf("outs of %s at fork %s: %w", st.Fqid, f, ErrNotStaged)
	}

	m := core.LazyArgumentMap{}

	for _, dir := range dirs {
		raw, err := shim.ReadBundle(dir)
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

func (s *stagedStore) element(root *syntax.CallGraphStage, f Fork, member string) (json.RawMessage, bool, error) {
	k := stagedKey(root.Fqid, f)

	dir, ok := s.elements[k]
	if !ok || !shim.IsBundle(dir) {
		return nil, false, nil
	}

	if slices.Contains(s.missing[k], member) {
		return nil, true, fmt.Errorf("%w: %s at fork %s of %s", errNoElement, member, f, root.Fqid)
	}

	raw, err := shim.ReadBundle(dir)
	if err != nil {
		return nil, false, fmt.Errorf("fork element of %s: %w", root.Fqid, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, fmt.Errorf("decode fork element of %s: %w", root.Fqid, err)
	}

	v, ok := payload[member]

	return v, ok, nil
}

// emptyStore is a task with nothing staged: what the call graph alone fixes.
type emptyStore struct{}

func (emptyStore) outs(st *syntax.CallGraphStage, f Fork) (core.LazyArgumentMap, error) {
	return nil, fmt.Errorf("outs of %s at fork %s: %w", st.Fqid, f, ErrNotStaged)
}

func (emptyStore) instances(*syntax.CallGraphStage, Fork) []syntax.CollectionIndex { return nil }

func (emptyStore) element(*syntax.CallGraphStage, Fork, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}
