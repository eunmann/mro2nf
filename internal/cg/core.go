package cg

import (
	"encoding/json"
	"fmt"

	"github.com/eunmann/mro2nf/internal/mrcore"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

// coreStore serves Martian core's resolution (mrcore) what one task staged.
type coreStore struct{ r *resolver }

// core is the resolver's run of Martian core's resolution over its store.
func (r *resolver) core() *mrcore.TopNode {
	if r.top == nil {
		r.top = mrcore.NewTop(r.g.root, r.g.types, coreStore{r})
	}

	return r.top
}

// coreFork is f as a core fork id.
func coreFork(f Fork) mrcore.ForkId {
	if len(f) == 0 {
		return nil
	}

	out := make(mrcore.ForkId, len(f))
	for i, p := range f {
		out[i] = mrcore.Part(p.Root, p.Index)
	}

	return out
}

// stageFork is fork's parts for the fork roots of st.
func stageFork(st *syntax.CallGraphStage, fork mrcore.ForkId) (Fork, error) {
	roots := st.ForkRoots()
	out := make(Fork, 0, len(roots))

	for _, root := range roots {
		ix, ok := fork.Index(root)
		if !ok {
			return nil, fmt.Errorf("%w: no index for %s in %s", errBadFork, root.Fqid, fork.GoString())
		}

		out = append(out, Part{Root: root, Index: concrete(ix)})
	}

	return out, nil
}

// cgFork is fork with each part's root, false where a part's split is no
// root's own.
func (g *Graph) cgFork(fork mrcore.ForkId) (Fork, bool) {
	out := make(Fork, 0, len(fork))

	for _, p := range fork {
		root, ok := g.splitRoots[p.Split]
		if !ok || p.Id.IndexSource() != nil {
			return nil, false
		}

		out = append(out, Part{Root: root, Index: concrete(p.Id)})
	}

	return out, true
}

func (c coreStore) Outs(st *syntax.CallGraphStage, fork mrcore.ForkId) (core.LazyArgumentMap, error) {
	f, err := stageFork(st, fork)
	if err != nil {
		return nil, err
	}

	return c.r.st.outs(st, f)
}

func (c coreStore) SplitElement(e *syntax.SplitExp, t syntax.Type, fork mrcore.ForkId, _ *mrcore.ForkSourcePart) (json.Marshaler, bool, error) {
	f, ok := c.r.g.cgFork(fork)
	if !ok {
		return nil, false, nil
	}

	p, ok := f.part(e.Call)
	if !ok {
		return nil, false, nil
	}

	return c.r.stagedElement(e, t, f, p)
}
