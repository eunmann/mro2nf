package gen

import "slices"

// The reductions Shrink applies. Each keeps references consistent where it
// can: a value that read something removed becomes null, and a call whose
// split reads something removed goes too. What it cannot keep consistent
// (a type that no longer matches) the compiler rejects, and Shrink skips it.

// nullify replaces each ref in e that dead matches with null.
func nullify(e *Expr, dead func(*Expr) bool) *Expr {
	if e.Kind == ExprRef {
		if dead(e) {
			return null()
		}

		return e
	}

	for i := range e.Elems {
		e.Elems[i] = nullify(e.Elems[i], dead)
	}

	return e
}

// killRefs nulls every value in pl that reads a ref dead matches, and drops
// every gate that does.
func killRefs(pl *Pipeline, dead func(*Expr) bool) {
	for _, c := range pl.Calls {
		for _, b := range c.Bindings {
			b.Value = nullify(b.Value, dead)
		}

		if c.Disabled != nil && dead(c.Disabled) {
			c.Disabled = nil
		}
	}

	for _, b := range pl.Return {
		b.Value = nullify(b.Value, dead)
	}
}

// killOutput nulls every read of output name of the callable callee.
func (p *Program) killOutput(callee, name string) {
	for _, pl := range p.Pipelines {
		ids := map[string]bool{}

		for _, c := range pl.Calls {
			if c.Callee == callee {
				ids[c.ID] = true
			}
		}

		killRefs(pl, func(e *Expr) bool { return ids[e.Ref] && len(e.Path) > 0 && e.Path[0] == name })
	}
}

// dropBinding removes the binding of input name from every call of callee.
func (p *Program) dropBinding(callee, name string) {
	for _, pl := range p.Pipelines {
		for _, c := range pl.Calls {
			if c.Callee == callee {
				c.Bindings = slices.DeleteFunc(c.Bindings, func(b *Binding) bool { return b.Name == name })
			}
		}
	}
}

func (p *Program) dropCall(pi, ci int) bool {
	pl := p.Pipelines[pi]
	dead := map[string]bool{pl.Calls[ci].ID: true}
	isDead := func(e *Expr) bool { return dead[e.Ref] }

	for changed := true; changed; {
		changed = false

		for _, c := range pl.Calls {
			if !dead[c.ID] && (splitsOn(c, isDead) || (c.Wildcard != nil && isDead(c.Wildcard))) {
				dead[c.ID] = true
				changed = true
			}
		}
	}

	pl.Calls = slices.DeleteFunc(pl.Calls, func(c *Call) bool { return dead[c.ID] })
	killRefs(pl, isDead)

	return true
}

func splitsOn(c *Call, dead func(*Expr) bool) bool {
	return slices.ContainsFunc(c.Bindings, func(b *Binding) bool { return b.Split && reads(b.Value, dead) })
}

// dropDeadCalls removes every call nothing reads, to a fixed point.
func dropDeadCalls(p *Program) bool {
	dropped := false

	for _, pl := range p.Pipelines {
		for {
			i := slices.IndexFunc(pl.Calls, func(c *Call) bool { return !callRead(pl, c.ID) })
			if i < 0 {
				break
			}

			pl.Calls = slices.Delete(pl.Calls, i, i+1)
			dropped = true
		}
	}

	return dropped
}

func (p *Program) dropReturn(pi, i int) bool {
	pl := p.Pipelines[pi]
	name := pl.Out[i].Name
	pl.Out = slices.Delete(pl.Out, i, i+1)
	pl.Return = slices.DeleteFunc(pl.Return, func(b *Binding) bool { return b.Name == name })
	p.killOutput(pl.Name, name)

	return true
}

func (p *Program) dropStageOut(si, i int) bool {
	st := p.Stages[si]
	name := st.Out[i].Name
	st.Out = slices.Delete(st.Out, i, i+1)
	p.killOutput(st.Name, name)

	return true
}

func (p *Program) dropStageIn(si, i int) bool {
	st := p.Stages[si]
	name := st.In[i].Name
	st.In = slices.Delete(st.In, i, i+1)
	p.dropBinding(st.Name, name)

	return true
}

func (p *Program) dropPipelineIn(pi, i int) bool {
	pl := p.Pipelines[pi]
	name := pl.In[i].Name
	pl.In = slices.Delete(pl.In, i, i+1)
	killRefs(pl, func(e *Expr) bool { return e.Ref == selfRef && len(e.Path) > 0 && e.Path[0] == name })
	p.dropBinding(pl.Name, name)

	if pl == p.EntryPipeline() {
		p.Entry = slices.DeleteFunc(p.Entry, func(b *Binding) bool { return b.Name == name })
	}

	return true
}

func (p *Program) dropDisabled(pi, ci int) bool {
	c := p.Pipelines[pi].Calls[ci]
	if c.Disabled == nil {
		return false
	}

	c.Disabled = nil

	return true
}

// dropField removes a struct field, its key from every struct literal, and
// every ref that projects it. Field names are unique in a program.
func (p *Program) dropField(ti, i int) bool {
	s := p.Structs[ti]
	name := s.Fields[i].Name
	s.Fields = slices.Delete(s.Fields, i, i+1)

	for _, slot := range p.exprSlots() {
		(*slot.e).walk(func(x *Expr) {
			if k := slices.Index(x.Keys, name); x.Kind == ExprStruct && k >= 0 {
				x.Keys = slices.Delete(x.Keys, k, k+1)
				x.Elems = slices.Delete(x.Elems, k, k+1)
			}
		})
	}

	for _, pl := range p.Pipelines {
		killRefs(pl, func(e *Expr) bool { return len(e.Path) > 1 && slices.Contains(e.Path[1:], name) })
	}

	return true
}

// tidy removes what a reduction left unused: pipeline inputs nothing reads
// (which Martian rejects), then uncalled stages and pipelines, then
// unreferenced structs and filetypes.
func (p *Program) tidy() {
	for changed := true; changed; {
		changed = false

		for pi, pl := range p.Pipelines {
			if i := slices.IndexFunc(pl.In, func(q Param) bool { return !p.inputUsed(pl, q.Name) }); i >= 0 {
				p.dropPipelineIn(pi, i)
				changed = true
			}
		}
	}

	called := map[string]bool{p.EntryPipeline().Name: true}

	for _, pl := range p.Pipelines {
		for _, c := range pl.Calls {
			called[c.Callee] = true
		}
	}

	p.Stages = slices.DeleteFunc(p.Stages, func(s *Stage) bool { return !called[s.Name] })
	p.Pipelines = slices.DeleteFunc(p.Pipelines, func(pl *Pipeline) bool { return !called[pl.Name] })
	p.tidyTypes()
}

func (p *Program) tidyTypes() {
	for {
		used := p.typeBases()
		n := len(p.Structs)
		p.Structs = slices.DeleteFunc(p.Structs, func(s *Struct) bool { return !used[s.Name] })

		if len(p.Structs) == n {
			p.FileTypes = slices.DeleteFunc(p.FileTypes, func(ft string) bool { return !used[ft] })

			return
		}
	}
}

// typeBases is every base type a declared parameter, output or field names.
func (p *Program) typeBases() map[string]bool {
	used := map[string]bool{}
	add := func(ps []Param) {
		for _, q := range ps {
			used[q.Type.Base] = true
		}
	}

	for _, s := range p.Structs {
		add(s.Fields)
	}

	for _, s := range p.Stages {
		add(s.In)
		add(s.Out)
		add(s.ChunkIn)
		add(s.ChunkOut)
	}

	for _, pl := range p.Pipelines {
		add(pl.In)
		add(pl.Out)
	}

	return used
}
