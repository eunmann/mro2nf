package gen

import "slices"

const seenOut = "seen"

// callNewStage declares a stage typed after the values it is bound to, and
// calls it, as a map call when mapped and a collection source exists.
func (pg *pipeGen) callNewStage(mapped bool) {
	g := pg.g
	st := &Stage{Name: g.name("ST")}
	c := &Call{Callee: st.Name, ID: st.Name}

	mode := forkNone
	if mapped {
		mode = pg.splitInputs(c, &st.In)
	}

	for range g.between(0, maxNewStageInputs) {
		name := g.name("i")
		e, t := pg.anyValue()
		c.Bindings = append(c.Bindings, &Binding{Name: name, Value: e})
		st.In = append(st.In, Param{Name: name, Type: t})
	}

	g.stageBody(st, mode)
	g.prog.Stages = append(g.prog.Stages, st)
	pg.finishCall(c, mode)
}

// stageBody gives st its outputs (a seen string recording the inputs the
// stub received, and up to maxStageOuts typed outputs), maybe a split with
// chunk parameters, and maybe a data-dependent failure.
func (g *generator) stageBody(st *Stage, mode forkMode) {
	st.Out = []Param{{Name: seenOut, Type: Type{Base: typeString}}}

	for range g.between(1, maxStageOuts) {
		st.Out = append(st.Out, Param{Name: g.name("o"), Type: g.randType(mode != forkMap)})
	}

	// A bool output of an ungated stage call is what a call-output disable
	// gate can read.
	if g.chance(pctBoolOut) {
		st.Out = append(st.Out, Param{Name: g.name("o"), Type: boolType()})
	}

	phases := []string{"main"}

	if g.chance(pctSplitStage) {
		st.Split = true

		for range g.between(1, maxChunkIns) {
			st.ChunkIn = append(st.ChunkIn, Param{Name: g.name("c"), Type: g.randType(true)})
		}

		for range g.between(0, maxChunkOuts) {
			st.ChunkOut = append(st.ChunkOut, Param{Name: g.name("d"), Type: g.randType(true)})
		}

		phases = append(phases, "split", "join")
	}

	// One failing stage at most: with two, which of them a run reports
	// depends on which the runtime starts before it stops on the first.
	if !g.failing && g.chance(pctFailStage) {
		g.failing = true
		kind := FailAssert

		if g.chance(pctFailError) {
			kind = FailError
		}

		st.Fail = &Fail{Kind: kind, Phase: phases[g.index(len(phases))], Mod: g.between(minFailMod, maxFailMod)}
	}
}

// splitInputs adds a split binding over a collection source, and sometimes
// a second one zipped with it, declaring each as an input of the element
// type. It returns the fork mode.
func (pg *pipeGen) splitInputs(c *Call, params *[]Param) forkMode {
	g := pg.g

	src, ok := pg.collection(nil, false, nil)
	if !ok {
		return forkNone
	}

	srcs := []source{src}
	if g.chance(pctZip) {
		srcs = append(srcs, pg.zipPartner(src))
	}

	for _, s := range srcs {
		name := g.name("i")
		c.Bindings = append(c.Bindings, &Binding{Name: name, Split: true, Value: s.expr})
		*params = append(*params, Param{Name: name, Type: s.typ.Elem()})
	}

	return modeOf(src.typ)
}

// collection draws a source a map call can split: an array, or a map at
// the top level. want fixes the element type; arrayOnly rules out a per-key
// fork; elemOK, when set, must accept the element type. Without a fitting
// source in scope it makes one: a fresh input or a non-empty literal.
func (pg *pipeGen) collection(want *Type, arrayOnly bool, elemOK func(Type) bool) (source, bool) {
	g := pg.g
	fits := func(t Type) bool {
		switch {
		case !t.IsArray() && !t.IsMap(), arrayOnly && t.IsMap():
			return false
		case want != nil:
			return t.Elem() == *want
		default:
			return elemOK == nil || elemOK(t.Elem())
		}
	}

	if s, ok := pg.pickSource(func(s source) bool { return fits(s.typ) }); ok && g.chance(pctSplitFromScope) {
		return s, true
	}

	t, ok := pg.freshCollectionType(want, arrayOnly, fits)
	if !ok {
		return source{}, false
	}

	if g.chance(pctSplitFromInput) {
		return pg.input(t, nil), true
	}

	sh := shape{known: true, isMap: t.IsMap(), n: g.between(1, maxCollLen)}
	if sh.isMap {
		sh.keys = g.drawKeys(sh.n, t)
	}

	lit := g.value(t, sh)

	return source{expr: lit, typ: t, shape: sh, group: sh.group()}, true
}

func (pg *pipeGen) freshCollectionType(want *Type, arrayOnly bool, fits func(Type) bool) (Type, bool) {
	g := pg.g

	if want != nil {
		if m, ok := want.MapOf(); ok && !arrayOnly && g.chance(pctSplitMap) {
			return m, true
		}

		return want.ArrayOf(), true
	}

	for range collectionTypeTries {
		t := g.randType(!arrayOnly)
		if t.Scalar() {
			t.Arr = 1
		}

		if fits(t) {
			return t, true
		}
	}

	return Type{}, false
}

// zipPartner is a second split source with the same fork count as src: a
// source of the same group, a literal of src's known shape, or src itself.
func (pg *pipeGen) zipPartner(src source) source {
	g := pg.g
	same := func(s source) bool {
		return s.group != "" && s.group == src.group && !s.typ.Scalar() &&
			s.typ.IsMap() == src.typ.IsMap() && renderExpr(s.expr) != renderExpr(src.expr)
	}

	if s, ok := pg.pickSource(same); ok && g.chance(pctZipSameGroup) {
		return s
	}

	if src.shape.known && src.shape.n > 0 && g.chance(pctZipShapedLiteral) {
		elem := g.randType(!src.shape.isMap)
		t := elem.ArrayOf()

		if src.shape.isMap {
			t, _ = elem.MapOf()
		}

		return source{expr: g.value(t, src.shape), typ: t}
	}

	return src
}

// anyValue binds a new stage input: a source in scope (sometimes read as
// float where it is an int), a fan-in of one, a fresh pipeline input, or a
// literal. It returns the expression and the type to declare.
func (pg *pipeGen) anyValue() (*Expr, Type) {
	g := pg.g
	roll := g.roll()

	switch {
	case roll < scopeValueBelow:
		if s, ok := pg.pickSource(nil); ok {
			if s.typ.Base == typeInt && g.chance(pctIntAsFloat) {
				return s.expr, Type{Base: typeFloat, Arr: s.typ.Arr, Map: s.typ.Map}
			}

			return s.expr, s.typ
		}
	case roll < fanInValueBelow:
		if s, ok := pg.pickSource(nil); ok {
			t := s.typ.ArrayOf()
			if m, isMap := s.typ.MapOf(); isMap && g.chance(pctMapFanIn) {
				t = m
			}

			return pg.fanIn(t, s.expr), t
		}
	case roll < inputValueBelow:
		t := g.randType(true)

		return pg.input(t, nil).expr, t
	}

	t := g.randType(true)

	return g.literal(t), t
}

// fanIn is a collection literal of type t whose elements are first, then
// sources of the element type or literals.
func (pg *pipeGen) fanIn(t Type, first *Expr) *Expr {
	g := pg.g
	elem := t.Elem()
	n := g.between(minFanIn, maxFanIn)
	e := &Expr{Kind: ExprArray}

	if t.IsMap() {
		e.Kind = ExprMap
		e.Keys = g.drawKeys(n, t)
	}

	for i := range n {
		switch s, ok := pg.pickSource(ofType(elem)); {
		case i == 0 && first != nil:
			e.Elems = append(e.Elems, first)
		case ok && g.chance(pctFanInSource):
			e.Elems = append(e.Elems, s.expr)
		default:
			e.Elems = append(e.Elems, g.literal(elem))
		}
	}

	return e
}

// bindValue binds an input of an existing callee: a source of its type, a
// fan-in, a fresh pipeline input, or a literal. A sub pipeline's bool
// inputs may gate its calls, so they get a non-null bool.
func (pg *pipeGen) bindValue(t Type, pipelineCallee bool) *Expr {
	g := pg.g

	if pipelineCallee && t == boolType() {
		return pg.safeBool()
	}

	scope, fanIn, input := bindBands(pipelineCallee)
	roll := g.roll()

	switch {
	case roll < scope:
		if s, ok := pg.scopeValue(t, pipelineCallee); ok {
			return s.expr
		}
	case roll < fanIn && (t.IsArray() || t.IsMap()):
		return pg.fanIn(t, nil)
	case roll < input:
		return pg.input(t, nil).expr
	}

	return g.literal(t)
}

// bindBands are bindValue's bands. A sub's call sites bind known values (an
// entry input, a literal) more often than a stage's, since what a sub does
// with an input can hinge on whether the value is known before the run
// (#241).
func bindBands(pipelineCallee bool) (int, int, int) {
	if pipelineCallee {
		return scopeBindSubBelow, fanInBindSubBelow, inputBindSubBelow
	}

	return scopeBindBelow, fanInBindBelow, inputBindBelow
}

// scopeValue draws a source of type t, for a sub's call site mostly a
// call's, so its sites differ in whether the value is known before the run.
func (pg *pipeGen) scopeValue(t Type, preferCalls bool) (source, bool) {
	if preferCalls {
		fromCall := func(s source) bool { return s.typ == t && s.expr.Ref != selfRef }
		if s, ok := pg.pickSource(fromCall); ok && pg.g.chance(pctSubBindFromCall) {
			return s, true
		}
	}

	return pg.pickSource(ofType(t))
}

// callNewSub generates a sub pipeline seeded with inputs typed after values
// in scope, and calls it, as a map call over a collection when mapped.
func (pg *pipeGen) callNewSub(mapped bool) {
	g := pg.g

	var (
		seed  []Type
		split *source
	)

	if mapped {
		notBool := func(t Type) bool { return t != boolType() }
		if s, ok := pg.collection(nil, false, notBool); ok {
			split = &s
			seed = append(seed, s.typ.Elem())
		}
	}

	// A sub usually gets a collection input typed after a call's output,
	// which its own map calls split: how a sub forks then depends on what
	// each call site binds, a value known before the run or a call's.
	fromCall := func(s source) bool { return !s.typ.Scalar() && s.expr.Ref != selfRef }
	if s, ok := pg.pickSource(fromCall); ok && g.chance(pctCollectionSeed) {
		seed = append(seed, s.typ)
	}

	for range g.between(0, maxSubSeeds) {
		if s, ok := pg.pickSource(nil); ok {
			seed = append(seed, s.typ)
		}
	}

	mode := forkNone
	if split != nil {
		mode = modeOf(split.typ)
	}

	sub := g.pipeline(g.name("P"), pg.depth+1, seed, mode == forkMap)
	g.prog.Pipelines = append(g.prog.Pipelines, sub.p)
	c := &Call{Callee: sub.p.Name, ID: sub.p.Name}

	// The sub drops inputs its body never read, the split one among them.
	if split != nil && paramIndex(sub.p.In, sub.seeds[0]) < 0 {
		split, mode = nil, forkNone
	}

	for _, in := range sub.p.In {
		if split != nil && in.Name == sub.seeds[0] {
			c.Bindings = append(c.Bindings, &Binding{Name: in.Name, Split: true, Value: split.expr})

			continue
		}

		c.Bindings = append(c.Bindings, &Binding{Name: in.Name, Value: pg.bindValue(in.Type, true)})
	}

	pg.finishCall(c, mode)
}

// callExisting calls a declared stage or sub pipeline again under a fresh
// alias, binding its inputs by type.
func (pg *pipeGen) callExisting(mapped bool) {
	g := pg.g

	stages := make([]string, 0, len(g.prog.Stages))
	for _, s := range g.prog.Stages {
		stages = append(stages, s.Name)
	}

	subs := make([]string, 0, len(g.prog.Pipelines))
	for _, p := range g.prog.Pipelines {
		subs = append(subs, p.Name)
	}

	// A sub pipeline called from several sites is where per-site semantics
	// (#241) can go wrong, so subs are drawn as often as all stages together.
	names := stages
	if len(subs) > 0 && (len(stages) == 0 || g.chance(pctPickSub)) {
		names = subs
	}

	callee := names[g.index(len(names))]
	in, out := g.prog.calleeIO(callee)
	isPipe := g.prog.pipeline(callee) != nil
	c := &Call{Callee: callee, ID: g.name("C")}
	mode, splitAt := forkNone, -1

	if at := g.index(len(in) + 1); mapped && at < len(in) && (!isPipe || in[at].Type != boolType()) {
		pt := in[at].Type
		mapOut := slices.ContainsFunc(out, func(o Param) bool { return o.Type.Map > 0 })

		if s, ok := pg.collection(&pt, mapOut, nil); ok {
			c.Bindings = append(c.Bindings, &Binding{Name: in[at].Name, Split: true, Value: s.expr})
			mode, splitAt = modeOf(s.typ), at
		}
	}

	for i, p := range in {
		if i != splitAt {
			c.Bindings = append(c.Bindings, &Binding{Name: p.Name, Value: pg.bindValue(p.Type, isPipe)})
		}
	}

	pg.finishCall(c, mode)
}

// callWildcard calls a new stage bound with `* = self` (its inputs are some
// of the pipeline's) or `* = CALL` (its inputs are some of an earlier call's
// outputs, by name). It reports false when the pipeline has neither.
func (pg *pipeGen) callWildcard() bool {
	g := pg.g
	st := &Stage{Name: g.name("ST")}
	c := &Call{Callee: st.Name, ID: st.Name}

	switch {
	case len(pg.p.Calls) > 0 && g.chance(pctWildcardFromCall):
		from := pg.p.Calls[g.index(len(pg.p.Calls))]
		_, outs := g.prog.calleeIO(from.Callee)

		for _, o := range outs {
			if t, ok := o.Type.lift(pg.modes[from.ID]); ok && o.Name != seenOut && g.chance(pctWildcardParam) {
				st.In = append(st.In, Param{Name: o.Name, Type: t})
			}
		}

		c.Wildcard = ref(from.ID)
	case len(pg.p.In) > 0:
		for _, in := range pg.p.In {
			if g.chance(pctWildcardParam) {
				st.In = append(st.In, in)
			}
		}

		c.Wildcard = ref(selfRef)
	}

	if len(st.In) == 0 {
		return false
	}

	g.stageBody(st, forkNone)
	g.prog.Stages = append(g.prog.Stages, st)
	pg.finishCall(c, forkNone)

	return true
}
