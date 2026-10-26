package cg

import (
	"maps"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

// bindingWalk walks bindings as the stage outs they read, the one walk
// both a reader's reads (Read) and the instances they read (ReadPattern)
// come from, so the two cannot disagree on what a binding reads. It visits
// each reference to stage outs with the splits over the reader's roots that
// enclose it.
type bindingWalk struct {
	g      *Graph
	reader *readerSplits
	visit  func(ref *syntax.RefExp, ctx []splitCtx)
}

// splitCtx is an enclosing split over the reader root root, at its element
// at idx, the ordinal ord of the n elements of the split's literal, or at
// the merge of root merge's forks, whose elements no ordinal names.
type splitCtx struct {
	root  *syntax.CallGraphStage
	idx   syntax.CollectionIndex
	keys  map[string]syntax.Exp
	ord   int
	n     int
	merge *syntax.CallGraphStage
}

// readerSplits are the splits of one reader's bindings the walk tells
// apart: those a staged element serves, and those over the reader's own
// roots, own. A nil one is the pipeline outs', which have neither.
type readerSplits struct {
	served func(*syntax.SplitExp) bool
	own    syntax.ForkRootList
}

// readerSplitsOf are st's splits: a split of a stage output other than a
// root's own source (partnerOf), or of the output a stage-fed root's forks
// come from, is served by the staged partner or fork element.
func (g *Graph) readerSplitsOf(st *syntax.CallGraphStage) *readerSplits {
	in := &readerSplits{own: st.ForkRoots()}
	in.served = func(x *syntax.SplitExp) bool {
		if _, partnered := g.partnerOf(st, x); partnered {
			return true
		}

		ref, ok := x.Value.(*syntax.RefExp)
		r := in.root(x.Call)

		return ok && r != nil && g.ownSource(r, ref)
	}

	return in
}

// forkRoots are the roots the reader is forked on, none for the pipeline's
// outs.
func (in *readerSplits) forkRoots() syntax.ForkRootList {
	if in == nil {
		return nil
	}

	return in.own
}

// root is the reader's root on call, nil when it is not forked on call.
func (in *readerSplits) root(call *syntax.CallStm) *syntax.CallGraphStage {
	own := in.forkRoots()

	i := slices.IndexFunc(own, func(r *syntax.CallGraphStage) bool { return r.Call() == call })
	if i < 0 {
		return nil
	}

	return own[i]
}

func (in *readerSplits) isServed(x *syntax.SplitExp) bool {
	return in != nil && in.served(x)
}

func (w *bindingWalk) exp(e syntax.Exp, ctx []splitCtx) {
	switch x := e.(type) {
	case *syntax.RefExp:
		w.visit(x, ctx)
	case *syntax.SplitExp:
		w.split(x, ctx)
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			w.exp(v, ctx)
		}
	case *syntax.MapExp:
		for _, k := range slices.Sorted(maps.Keys(x.Value)) {
			w.exp(x.Value[k], ctx)
		}
	case *syntax.MergeExp:
		w.merge(x, ctx)
	case *syntax.DisabledExp:
		w.exp(x.Disabled, ctx)
		w.exp(x.Value, ctx)
	}
}

// split visits x: of a split a staged element serves, only its gate; of
// one over a reader root, the collection it forks.
func (w *bindingWalk) split(x *syntax.SplitExp, ctx []splitCtx) {
	switch r := w.reader.root(x.Call); {
	case w.reader.isServed(x):
		if d, ok := x.Value.(*syntax.DisabledExp); ok {
			w.exp(d.Disabled, ctx)
		}
	case r != nil:
		w.collection(r, x.Value, ctx)
	default:
		w.exp(x.Value, ctx)
	}
}

// merge visits x where the reader resolves it. A merge over a root whose
// forks a stage fixes takes them from the root's source, as mrp does: the
// instances it gathers are no count of them, as a disabled one posts none.
// It reads what counting them reads (Graph.countReads), not the source's
// every value.
func (w *bindingWalk) merge(x *syntax.MergeExp, ctx []splitCtx) {
	if w.g.mergeGathersNone(x, w.reader.forkRoots()) {
		return
	}

	w.exp(x.Value, ctx)

	if x.ForkNode != nil {
		w.visit(x.ForkNode, ctx)
	}

	w.countReads(x, ctx)
}

// countReads visits what counting the forks the merge x gathers reads
// (Graph.mergeCounted, Graph.countReads), each at the indices of the outer
// roots the merge's constraint fixes, where the merge resolves it
// (mergeIndices).
func (w *bindingWalk) countReads(x *syntax.MergeExp, ctx []splitCtx) {
	root, ok := w.g.mergeCounted(x, w.reader.forkRoots())
	if !ok {
		return
	}

	constraint := mergeConstraint(x)

	for _, r := range w.g.countReads(root) {
		at := *r
		at.Forks = maps.Clone(r.Forks)

		if at.Forks == nil {
			at.Forks = map[*syntax.CallStm]syntax.CollectionIndex{}
		}

		if constraint != nil {
			for c, ix := range constraint.Forks {
				if ix != nil && ix.IndexSource() == nil {
					if was, ok := at.Forks[c]; !ok || was == nil || was.IndexSource() != nil {
						at.Forks[c] = ix
					}
				}
			}
		}

		w.visit(&at, ctx)
	}
}

// collection visits v, the collection a split over the reader root r forks,
// element by element. A merge there is of the forks at the reader's own
// element, one instance its fork names, so it reads the merged value and
// nothing counting the merge's forks reads, which grows with their number.
func (w *bindingWalk) collection(r *syntax.CallGraphStage, v syntax.Exp, ctx []splitCtx) {
	switch x := v.(type) {
	case *syntax.ArrayExp:
		for i, el := range x.Value {
			w.exp(el, append(slices.Clip(ctx), splitCtx{root: r, idx: ArrayIndex(i), ord: i, n: len(x.Value)}))
		}
	case *syntax.MapExp:
		keys := slices.Sorted(maps.Keys(x.Value))
		for i, k := range keys {
			w.exp(x.Value[k], append(slices.Clip(ctx), splitCtx{root: r, idx: MapKey(k), keys: x.Value, ord: i, n: len(keys)}))
		}
	case *syntax.MergeExp:
		if !w.g.mergeGathersNone(x, w.reader.forkRoots()) {
			w.exp(x.Value, append(slices.Clip(ctx), splitCtx{root: r, merge: x.Call}))
		}
	case *syntax.DisabledExp:
		w.exp(x.Disabled, ctx)
		w.collection(r, x.Value, ctx)
	case *syntax.SplitExp:
		if o := w.reader.root(x.Call); o != nil && !w.reader.isServed(x) {
			w.nested(r, o, x.Value, ctx)
		} else {
			w.split(x, ctx)
		}
	default:
		w.exp(v, ctx)
	}
}

// nested visits v, a collection of r's collections, one per fork of the
// reader root o.
func (w *bindingWalk) nested(r, o *syntax.CallGraphStage, v syntax.Exp, ctx []splitCtx) {
	switch x := v.(type) {
	case *syntax.ArrayExp:
		for i, el := range x.Value {
			w.collection(r, el, append(slices.Clip(ctx), splitCtx{root: o, idx: ArrayIndex(i), ord: i, n: len(x.Value)}))
		}
	case *syntax.MapExp:
		keys := slices.Sorted(maps.Keys(x.Value))
		for i, k := range keys {
			w.collection(r, x.Value[k], append(slices.Clip(ctx), splitCtx{root: o, idx: MapKey(k), keys: x.Value, ord: i, n: len(keys)}))
		}
	case *syntax.DisabledExp:
		w.exp(x.Disabled, ctx)
		w.nested(r, o, x.Value, ctx)
	default:
		w.collection(o, v, ctx)
	}
}
