package gen

import "slices"

// srcKind is what a source expression reads: a pipeline input, a call
// output, a whole call, or a field projected out of one of those.
type srcKind int

const (
	srcSelf srcKind = iota
	srcOut
	srcWhole
	srcProj
	numSrcKinds
)

// srcWeight biases the draw toward call outputs, so the dataflow is deep.
func srcWeight(k srcKind) int {
	switch k {
	case srcSelf:
		return weightSelf
	case srcOut:
		return weightOut
	case srcWhole:
		return weightWhole
	default:
		return weightProjection
	}
}

// source is a typed value a binding in the pipeline may read.
type source struct {
	expr *Expr
	typ  Type
	kind srcKind
	// group is shared by sources whose outer collections always have the
	// same length or keys, so a map call may zip them.
	group string
	// shape is the outer shape of a literal-backed source, when known.
	shape shape
	// safe marks a bool that is never null at run time; only such a value
	// may gate a call, as a null disable flag is a known divergence (#253).
	safe bool
}

// pipeGen builds one pipeline's body.
type pipeGen struct {
	g     *generator
	p     *Pipeline
	depth int
	scope []source
	entry []*Binding
	modes map[string]forkMode
	// seeds names the inputs declared for the caller's seed types, in order.
	seeds []string
}

func boolType() Type { return Type{Base: typeBool} }

// pipeline generates a pipeline at the given nesting depth (0 is the entry
// pipeline), with an input of each seed type. noMapOut keeps map-typed
// outputs out, for a pipeline its caller maps per key.
func (g *generator) pipeline(name string, depth int, seedIns []Type, noMapOut bool) *pipeGen {
	pg := &pipeGen{g: g, p: &Pipeline{Name: name}, depth: depth, modes: map[string]forkMode{}}

	for _, t := range seedIns {
		pg.seeds = append(pg.seeds, pg.input(t, nil).expr.Path[0])
	}

	if depth == 0 {
		for range g.between(1, maxEntryInputs) {
			pg.input(g.randType(true), nil)
		}
	}

	lo, hi := 1, maxSubCalls

	switch depth {
	case 0:
		lo, hi = minEntryCalls, maxEntryCalls
	case maxDepth:
		hi = maxDeepCalls
	}

	for range g.between(lo, hi) {
		pg.addCall()
	}

	pg.returns(noMapOut)
	pg.p.In = slices.DeleteFunc(pg.p.In, func(q Param) bool { return !pg.g.prog.inputUsed(pg.p, q.Name) })
	pg.entry = slices.DeleteFunc(pg.entry, func(b *Binding) bool { return paramIndex(pg.p.In, b.Name) < 0 })

	return pg
}

// input declares a fresh pipeline input of type t. The entry pipeline's
// inputs are bound to lit (or a random literal) by the entry call; a sub
// pipeline's are bound by each caller, which binds every bool input to a
// non-null bool, so a sub may gate calls on any of them.
func (pg *pipeGen) input(t Type, lit *Expr) source {
	name := pg.g.name("p")
	pg.p.In = append(pg.p.In, Param{Name: name, Type: t})
	s := source{expr: ref(selfRef, name), typ: t, kind: srcSelf, safe: t == boolType()}

	if pg.depth == 0 {
		if lit == nil {
			lit = pg.g.literal(t)
		}

		// A null entry collection would make a split over it statically
		// null, a map call shape mro2nf rejects at transpile time.
		if lit.Kind == ExprNull && !t.Scalar() {
			lit = pg.g.value(t, shape{})
		}

		pg.entry = append(pg.entry, &Binding{Name: name, Value: lit})
		s.shape = shapeOf(lit)
		s.group = s.shape.group()
		s.safe = lit.Kind == ExprBool
	}

	pg.addSource(s)

	return s
}

func (pg *pipeGen) addSource(s source) {
	pg.scope = append(pg.scope, s)

	if s.kind != srcWhole {
		pg.addProjections(s, 0)
	}
}

// addProjections adds the fields reachable from a struct-typed source,
// maxProjectionDepth levels deep, through any arrays and the one map level
// mro2nf projects.
func (pg *pipeGen) addProjections(s source, depth int) {
	if depth >= maxProjectionDepth {
		return
	}

	fields, ok := pg.g.prog.fields(s.typ.Base)
	if !ok {
		return
	}

	for _, f := range fields {
		t, ok := s.typ.project(f.Type)
		if !ok {
			continue
		}

		ps := source{expr: ref(s.expr.Ref, append(slices.Clone(s.expr.Path), f.Name)...), typ: t, kind: srcProj}
		pg.scope = append(pg.scope, ps)
		pg.addProjections(ps, depth+1)
	}
}

// pickSource draws a source satisfying keep (any when nil): first its kind,
// by srcWeight among the kinds that have one, then uniformly within it.
func (pg *pipeGen) pickSource(keep func(source) bool) (source, bool) {
	var byKind [numSrcKinds][]source

	for _, s := range pg.scope {
		if keep == nil || keep(s) {
			byKind[s.kind] = append(byKind[s.kind], s)
		}
	}

	total := 0

	for k, ss := range byKind {
		if len(ss) > 0 {
			total += srcWeight(srcKind(k))
		}
	}

	if total == 0 {
		return source{}, false
	}

	roll := pg.g.r.IntN(total)

	for k, ss := range byKind {
		if len(ss) == 0 {
			continue
		}

		w := srcWeight(srcKind(k))
		if roll < w {
			return ss[pg.g.index(len(ss))], true
		}

		roll -= w
	}

	return source{}, false
}

func ofType(t Type) func(source) bool {
	return func(s source) bool { return s.typ == t }
}

func (pg *pipeGen) addCall() {
	g := pg.g
	roll := g.roll()

	switch {
	case roll < newSubBelow && pg.depth < maxDepth:
		pg.callNewSub(g.chance(pctMapNewSub))
	case roll < existingBelow && len(g.prog.Stages)+len(g.prog.Pipelines) > 0:
		pg.callExisting(g.chance(pctMapExisting))
	case roll < wildcardBelow:
		if !pg.callWildcard() {
			pg.callNewStage(false)
		}
	case pg.depth > 0:
		pg.callNewStage(g.chance(pctMapNewSubStage))
	default:
		pg.callNewStage(g.chance(pctMapNewStage))
	}
}

// finishCall maybe gates c, appends it, and adds its outputs, its whole
// value and their projections to the scope.
func (pg *pipeGen) finishCall(c *Call, mode forkMode) {
	g := pg.g

	if g.chance(pctGate) {
		c.Disabled = pg.gateRef()
	}

	pg.p.Calls = append(pg.p.Calls, c)
	pg.modes[c.ID] = mode

	_, outs := g.prog.calleeIO(c.Callee)
	isStage := g.prog.stage(c.Callee) != nil

	// A stage draws every top-level collection output of one invocation to
	// one shape, and a map call's outputs all have one element per fork.
	group := ""
	if isStage || mode != forkNone {
		group = "call:" + c.ID
	}

	for _, o := range outs {
		t, ok := o.Type.lift(mode)
		if !ok {
			continue
		}

		safe := isStage && mode == forkNone && c.Disabled == nil && t == boolType()
		pg.addSource(source{expr: ref(c.ID, o.Name), typ: t, kind: srcOut, group: group, safe: safe})
	}

	if t, ok := (Type{Base: c.Callee}).lift(mode); ok {
		pg.addSource(source{expr: ref(c.ID), typ: t, kind: srcWhole, group: group})
	}
}

// gateRef is a disable ref to a bool that is never null: a safe source, or a
// fresh bool input.
func (pg *pipeGen) gateRef() *Expr {
	fromCall := func(s source) bool { return s.safe && s.kind == srcOut }
	if s, ok := pg.pickSource(fromCall); ok && pg.g.chance(pctGateFromCall) {
		return s.expr
	}

	if s, ok := pg.pickSource(func(s source) bool { return s.safe }); ok && pg.g.chance(pctGateSafe) {
		return s.expr
	}

	return pg.input(boolType(), boolLit(pg.g.chance(pctTrue))).expr
}

// safeBool binds a sub pipeline's bool input: a safe source or a literal.
func (pg *pipeGen) safeBool() *Expr {
	if s, ok := pg.pickSource(func(s source) bool { return s.safe }); ok && pg.g.chance(pctSafeBool) {
		return s.expr
	}

	return boolLit(pg.g.chance(pctTrue))
}

// returns declares the pipeline's outputs, bound mostly to call outputs and
// whole calls.
func (pg *pipeGen) returns(noMapOut bool) {
	g := pg.g

	lo, hi := 1, maxSubReturns
	if pg.depth == 0 {
		lo, hi = minEntryReturns, maxEntryReturns
	}

	keep := func(s source) bool { return !noMapOut || s.typ.Map == 0 }

	for range g.between(lo, hi) {
		var (
			e *Expr
			t Type
		)

		if s, ok := pg.pickSource(keep); ok && !g.chance(pctLiteralReturn) {
			e, t = s.expr, s.typ
			if g.chance(pctFanInReturn) {
				t = s.typ.ArrayOf()
				e = pg.fanIn(t, s.expr)
			}
		} else {
			t = g.randType(!noMapOut)
			e = g.literal(t)
		}

		pg.addReturn(e, t)
	}

	// Every call's results reach an output or a consumer, so whatever a
	// call computes is compared with mrp's.
	for _, c := range pg.p.Calls {
		if callRead(pg.p, c.ID) {
			continue
		}

		of := func(s source) bool { return keep(s) && s.kind != srcProj && s.expr.Ref == c.ID }
		if s, ok := pg.pickSource(of); ok {
			pg.addReturn(s.expr, s.typ)
		}
	}
}

func (pg *pipeGen) addReturn(e *Expr, t Type) {
	name := pg.g.name("r")
	pg.p.Out = append(pg.p.Out, Param{Name: name, Type: t})
	pg.p.Return = append(pg.p.Return, &Binding{Name: name, Value: e})
}
