package gen

import (
	"slices"
	"sync"
)

// ShrinkOptions bounds a shrink.
type ShrinkOptions struct {
	// MaxEvals caps how many times the predicate runs.
	MaxEvals int
	// Parallel is how many candidates are evaluated at once.
	Parallel int
}

// Shrink delta-debugs p against fails, which reports whether a program
// still shows the divergence and must be safe for concurrent use. Each round
// tries the reductions of the current program in a fixed coarse-to-fine
// order (dead calls, outputs, calls, splits and gates, parameters and fields,
// then values) and moves to the first one that compiles and still fails. It
// stops when no reduction fails or the budget is spent, and returns the
// smallest failing program, marked Shrunk when it is smaller than p, and the
// evaluations spent. Candidates are judged
// in batches of opts.Parallel, but the first failing candidate in order
// wins, so the result does not depend on timing.
func Shrink(p *Program, fails func(*Program) bool, opts ShrinkOptions) (*Program, int) {
	cur := p.Clone()
	evals := 0

	for evals < opts.MaxEvals {
		next, spent := firstFailing(candidates(cur), fails, opts, opts.MaxEvals-evals)
		evals += spent

		if next == nil {
			break
		}

		cur = next
		cur.Shrunk = true
	}

	return cur, evals
}

func firstFailing(cands []*Program, fails func(*Program) bool, opts ShrinkOptions, budget int) (*Program, int) {
	batch := max(opts.Parallel, 1)
	spent := 0

	for start := 0; start < len(cands) && spent < budget; start += batch {
		end := min(start+batch, len(cands), start+budget-spent)
		results := make([]bool, end-start)

		var wg sync.WaitGroup

		for i := start; i < end; i++ {
			wg.Go(func() { results[i-start] = fails(cands[i]) })
		}

		wg.Wait()

		spent += end - start

		if i := slices.Index(results, true); i >= 0 {
			return cands[start+i], spent
		}
	}

	return nil, spent
}

// reduction mutates a program in place and reports whether it applied.
type reduction func(*Program) bool

// candidates is every distinct valid program one reduction away from p,
// tidied, in the order Shrink tries them.
func candidates(p *Program) []*Program {
	seen := map[string]bool{p.Key(): true}

	var out []*Program

	for _, r := range reductions(p) {
		c := p.Clone()
		if !r(c) {
			continue
		}

		c.tidy()

		k := c.Key()
		if seen[k] || Validate(c) != nil {
			continue
		}

		seen[k] = true

		out = append(out, c)
	}

	return out
}

func reductions(p *Program) []reduction {
	rs := []reduction{dropDeadCalls}
	rs = append(rs, perPipeline(p, func(pi int, pl *Pipeline) []reduction {
		return indexed(len(pl.Out), func(i int) reduction { return func(q *Program) bool { return q.dropReturn(pi, i) } })
	})...)
	rs = append(rs, perPipeline(p, func(pi int, pl *Pipeline) []reduction {
		return indexed(len(pl.Calls), func(i int) reduction {
			return func(q *Program) bool { return q.dropCall(pi, len(pl.Calls)-1-i) }
		})
	})...)
	rs = append(rs, stageReductions(p)...)
	rs = append(rs, perPipeline(p, func(pi int, pl *Pipeline) []reduction {
		var out []reduction

		for ci := range pl.Calls {
			out = append(out, func(q *Program) bool { return q.dropDisabled(pi, ci) })
		}

		return append(out, indexed(len(pl.In), func(i int) reduction {
			return func(q *Program) bool { return q.dropPipelineIn(pi, i) }
		})...)
	})...)
	rs = append(rs, paramReductions(p)...)

	return append(rs, valueReductions(p)...)
}

// perPipeline concatenates f's reductions for each pipeline, entry first.
func perPipeline(p *Program, f func(int, *Pipeline) []reduction) []reduction {
	var out []reduction

	for pi, pl := range slices.Backward(p.Pipelines) {
		out = append(out, f(pi, pl)...)
	}

	return out
}

func indexed(n int, f func(int) reduction) []reduction {
	out := make([]reduction, n)
	for i := range n {
		out[i] = f(i)
	}

	return out
}

func stageReductions(p *Program) []reduction {
	var out []reduction

	for si, s := range p.Stages {
		if s.Split {
			out = append(out, func(q *Program) bool { return q.unsplit(si) })
		}

		if s.Fail != nil {
			out = append(out, func(q *Program) bool { return q.dropFail(si) })
		}
	}

	return out
}

func paramReductions(p *Program) []reduction {
	var out []reduction

	for si, s := range p.Stages {
		out = append(out, indexed(len(s.Out), func(i int) reduction {
			// The seen record is what a shrunk fixture shows its divergence in.
			if s.Out[i].Name == seenOut {
				return func(*Program) bool { return false }
			}

			return func(q *Program) bool { return q.dropStageOut(si, i) }
		})...)
		out = append(out, indexed(len(s.In), func(i int) reduction { return func(q *Program) bool { return q.dropStageIn(si, i) } })...)
		out = append(out, indexed(len(s.ChunkIn), func(i int) reduction { return func(q *Program) bool { return q.dropChunkIn(si, i) } })...)
		out = append(out, indexed(len(s.ChunkOut), func(i int) reduction { return func(q *Program) bool { return q.dropChunkOut(si, i) } })...)
	}

	for ti, s := range p.Structs {
		out = append(out, indexed(len(s.Fields), func(i int) reduction { return func(q *Program) bool { return q.dropField(ti, i) } })...)
	}

	return out
}

// valueReductions blank each bound value, then drop each element of each
// collection literal, then replace each scalar literal with a plain one.
func valueReductions(p *Program) []reduction {
	slots := p.exprSlots()

	var blanks, drops, plains []reduction

	for si, s := range slots {
		if !s.split {
			blanks = append(blanks, func(q *Program) bool { return q.blankSlot(si) })
		}

		for ni, n := range nodes(*s.e) {
			minLen := 0
			if s.split && ni == 0 {
				minLen = 1
			}

			if n.Kind == ExprArray || n.Kind == ExprMap {
				drops = append(drops, indexed(len(n.Elems), func(i int) reduction {
					return func(q *Program) bool { return dropElem(nodes(*q.exprSlots()[si].e)[ni], i, minLen) }
				})...)
			}

			if plain, ok := plainScalar(n.Kind); ok && n.Text != plain {
				plains = append(plains, func(q *Program) bool { return setText(nodes(*q.exprSlots()[si].e)[ni], plain) })
			}
		}
	}

	return slices.Concat(blanks, drops, plains)
}

// plainScalar is the unremarkable literal a scalar of kind k shrinks to.
func plainScalar(k ExprKind) (string, bool) {
	switch k {
	case ExprInt:
		return "1", true
	case ExprFloat:
		return "0.5", true
	case ExprString:
		return "a", true
	default:
		return "", false
	}
}

func setText(e *Expr, text string) bool {
	e.Text = text

	return true
}

// blankSlot sets the value of slot si to null, or false for a gate input,
// and reports whether that changed it. A null value is already blank.
func (p *Program) blankSlot(si int) bool {
	s := p.exprSlots()[si]

	blank := null()
	if s.gate {
		blank = boolLit(false)
	}

	if old := *s.e; old.Kind == ExprNull || (old.Kind == blank.Kind && old.Text == blank.Text) {
		return false
	}

	*s.e = blank

	return true
}

func (p *Program) unsplit(si int) bool {
	st := p.Stages[si]
	st.Split, st.ChunkIn, st.ChunkOut = false, nil, nil

	return true
}

func (p *Program) dropFail(si int) bool {
	p.Stages[si].Fail = nil

	return true
}

func (p *Program) dropChunkIn(si, i int) bool {
	p.Stages[si].ChunkIn = slices.Delete(p.Stages[si].ChunkIn, i, i+1)

	return true
}

func (p *Program) dropChunkOut(si, i int) bool {
	p.Stages[si].ChunkOut = slices.Delete(p.Stages[si].ChunkOut, i, i+1)

	return true
}

func dropElem(n *Expr, i, minLen int) bool {
	if len(n.Elems) <= minLen {
		return false
	}

	n.Elems = slices.Delete(n.Elems, i, i+1)
	if n.Kind == ExprMap {
		n.Keys = slices.Delete(n.Keys, i, i+1)
	}

	return true
}

// nodes lists e and its nested expressions in walk order.
func nodes(e *Expr) []*Expr {
	var out []*Expr

	e.walk(func(x *Expr) { out = append(out, x) })

	return out
}

// exprSlot is a place a bound value lives, whether it is split, and whether
// it binds a gate input.
type exprSlot struct {
	e     **Expr
	split bool
	gate  bool
}

// exprSlots lists every bound value in a fixed order: entry bindings, then
// each pipeline's call bindings and returns.
func (p *Program) exprSlots() []exprSlot {
	var out []exprSlot

	entry := p.EntryPipeline().Name
	for _, b := range p.Entry {
		out = append(out, exprSlot{e: &b.Value, gate: p.gateInput(entry, b.Name)})
	}

	for _, pl := range p.Pipelines {
		for _, c := range pl.Calls {
			for _, b := range c.Bindings {
				out = append(out, exprSlot{e: &b.Value, split: b.Split, gate: p.gateInput(c.Callee, b.Name)})
			}
		}

		for _, b := range pl.Return {
			out = append(out, exprSlot{e: &b.Value})
		}
	}

	return out
}
