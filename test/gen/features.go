package gen

import (
	"maps"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Features names the grammar constructs and corpus values p uses, read off
// the program itself, so it stays true of a shrunk program too.
func Features(p *Program) map[string]bool {
	f := map[string]bool{}

	for _, s := range p.Structs {
		f["type:struct"] = true

		for _, fd := range s.Fields {
			f["type:struct-of-struct"] = f["type:struct-of-struct"] || p.isStruct(fd.Type.Base)
			f["type:struct-with-file"] = f["type:struct-with-file"] || p.hasFiles(fd.Type)
		}
	}

	for _, s := range p.Stages {
		f["stage:split"] = f["stage:split"] || s.Split
		f["stage:fail"] = f["stage:fail"] || s.Fail != nil

		for _, ps := range [][]Param{s.In, s.Out, s.ChunkIn, s.ChunkOut} {
			for _, q := range ps {
				typeFeatures(p, q.Type, f)
			}
		}
	}

	for _, pl := range p.Pipelines {
		if pl != p.EntryPipeline() {
			f["pipeline:sub"] = true
		}

		for _, c := range pl.Calls {
			callFeatures(p, pl, c, f)
		}

		for _, b := range pl.Return {
			bindingFeatures(p, pl, b.Value, f)
		}
	}

	for _, b := range p.Entry {
		valueFeatures(b.Value, f)
	}

	maps.DeleteFunc(f, func(_ string, present bool) bool { return !present })

	return f
}

func (p *Program) isStruct(base string) bool {
	_, ok := p.fields(base)

	return ok
}

func typeFeatures(p *Program, t Type, f map[string]bool) {
	switch {
	case t.Map > 0:
		f["type:map"] = true
	case t.Arr > 1:
		f["type:array-2d"] = true
	case t.Arr == 1:
		f["type:array"] = true
	}

	switch {
	case t.Base == typePath:
		f["type:path"] = true
	case t.Base == typeFile:
		f["type:file"] = true
	case p.isFileBase(t.Base):
		f["type:filetype"] = true
	}
}

func callFeatures(p *Program, pl *Pipeline, c *Call, f map[string]bool) {
	if c.ID != c.Callee {
		f["call:alias"] = true
	}

	sub := p.pipeline(c.Callee)
	if sub != nil && p.stage(c.Callee) == nil {
		f["call:sub"] = true
	}

	if c.IsMap() {
		mapCallFeatures(p, pl, c, sub, f)
	}

	if c.Disabled != nil {
		f["disabled:"+map[bool]string{true: "self", false: "call"}[c.Disabled.Ref == selfRef]] = true
	}

	if c.Wildcard != nil {
		f["bind:wildcard-"+map[bool]string{true: "self", false: "call"}[c.Wildcard.Ref == selfRef]] = true
	}

	for _, b := range c.Bindings {
		bindingFeatures(p, pl, b.Value, f)

		if in, _ := p.calleeIO(c.Callee); !b.Split {
			if i := paramIndex(in, b.Name); i >= 0 && in[i].Type.Base == typeFloat && refIsInt(p, pl, b.Value) {
				f["bind:int-as-float"] = true
			}
		}
	}
}

func mapCallFeatures(p *Program, pl *Pipeline, c *Call, sub *Pipeline, f map[string]bool) {
	splits := 0

	for _, b := range c.Bindings {
		if b.Split {
			splits++
		}
	}

	f["call:map-zip"] = f["call:map-zip"] || splits > 1
	f["call:map-"+map[forkMode]string{forkArray: "array", forkMap: "map", forkNone: "?"}[p.callMode(pl, c)]] = true
	f["call:map-disabled"] = f["call:map-disabled"] || c.Disabled != nil

	if st := p.stage(c.Callee); st != nil && st.Split {
		f["call:map-split-stage"] = true
	}

	if sub != nil {
		f["call:map-sub"] = true

		for _, inner := range sub.Calls {
			f["call:map-nested"] = f["call:map-nested"] || inner.IsMap()
		}
	}
}

func refIsInt(p *Program, pl *Pipeline, e *Expr) bool {
	t, ok := p.exprType(pl, e)

	return ok && e.Kind == ExprRef && t.Base == typeInt
}

// bindingFeatures records the ref shapes in a bound value and its literals.
func bindingFeatures(p *Program, pl *Pipeline, e *Expr, f map[string]bool) {
	if e.Kind == ExprArray || e.Kind == ExprMap {
		for _, el := range e.Elems {
			if el.Kind == ExprRef {
				f["bind:fanin-"+string(e.Kind)] = true
			}
		}
	}

	e.walk(func(x *Expr) {
		if x.Kind != ExprRef {
			return
		}

		refFeatures(p, pl, x, f)
	})
	valueFeatures(e, f)
}

func refFeatures(p *Program, pl *Pipeline, x *Expr, f map[string]bool) {
	if x.Ref != selfRef && len(x.Path) == 0 {
		f["bind:whole-ref"] = true

		if c := callByID(pl, x.Ref); c != nil {
			f["bind:whole-ref-mapped"] = f["bind:whole-ref-mapped"] || c.IsMap()
			f["bind:whole-ref-disabled"] = f["bind:whole-ref-disabled"] || c.Disabled != nil
		}
	}

	// Path[0] picks the input or output; each later segment projects a field
	// out of the value the segments before it reach.
	for i := 1; i < len(x.Path); i++ {
		t, ok := p.exprType(pl, ref(x.Ref, x.Path[:i]...))
		if !ok {
			continue
		}

		f["bind:projection"] = true
		f["bind:projection-through-array"] = f["bind:projection-through-array"] || t.Arr > 0
		f["bind:projection-through-map"] = f["bind:projection-through-map"] || t.Map > 0
	}
}

// The value boundaries Features reports a literal reaching: past these a
// float64 loses integer precision, and a JSON encoder writes a float in
// exponent form.
const (
	maxExactInt  = 1 << 53
	bigFloat     = 1e6
	tinyFloat    = 1e-6
	markerPrefix = "@mre:"
)

// valueFeatures records the corpus boundaries the literals in e reach.
func valueFeatures(e *Expr, f map[string]bool) {
	e.walk(func(x *Expr) {
		switch x.Kind {
		case ExprNull:
			f["value:null"] = true
		case ExprArray:
			f["value:empty-array"] = f["value:empty-array"] || len(x.Elems) == 0
		case ExprMap:
			f["value:empty-map"] = f["value:empty-map"] || len(x.Elems) == 0

			for _, k := range x.Keys {
				keyFeatures(k, f)
			}
		case ExprFloat:
			floatFeatures(x.Text, f)
		case ExprInt:
			n, err := strconv.ParseInt(x.Text, 10, 64)
			f["value:int-beyond-2^53"] = f["value:int-beyond-2^53"] || (err == nil && (n > maxExactInt || n < -maxExactInt))
		case ExprString:
			f["value:marker-string"] = f["value:marker-string"] || strings.HasPrefix(x.Text, markerPrefix)
		default:
		}
	})
}

func floatFeatures(text string, f map[string]bool) {
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return
	}

	switch {
	case v == 0 && math.Signbit(v):
		f["value:negative-zero"] = true
	case math.Abs(v) >= bigFloat:
		f["value:float-at-least-1e6"] = true
	case v != 0 && math.Abs(v) <= tinyFloat:
		f["value:float-at-most-1e-6"] = true
	}
}

func keyFeatures(k string, f map[string]bool) {
	switch {
	case k == "":
		f["key:empty"] = true
	case k == "~":
		f["key:tilde"] = true
	case strings.Contains(k, "~"):
		f["key:contains-tilde"] = true
	case strings.Contains(k, "\n"):
		f["key:newline"] = true
	case strings.IndexFunc(k, func(r rune) bool { return r > unicode.MaxASCII }) >= 0:
		f["key:unicode"] = true
	case strings.Trim(k, "0123456789") == "":
		f["key:numeric"] = true
	}
}

// refRoot is the type of the value the ref e starts from (a pipeline input,
// or a whole call under its fork) and the fields it projects from there.
func (p *Program) refRoot(pl *Pipeline, e *Expr) (Type, []string, bool) {
	if e.Ref != selfRef {
		c := callByID(pl, e.Ref)
		if c == nil {
			return Type{}, nil, false
		}

		t, ok := (Type{Base: c.Callee}).lift(p.callMode(pl, c))

		return t, e.Path, ok
	}

	if len(e.Path) == 0 {
		return Type{}, nil, false
	}

	i := paramIndex(pl.In, e.Path[0])
	if i < 0 {
		return Type{}, nil, false
	}

	return pl.In[i].Type, e.Path[1:], true
}

func callByID(pl *Pipeline, id string) *Call {
	for _, c := range pl.Calls {
		if c.ID == id {
			return c
		}
	}

	return nil
}

// callMode is how c forks: the kind of its first split source.
func (p *Program) callMode(pl *Pipeline, c *Call) forkMode {
	for _, b := range c.Bindings {
		if !b.Split {
			continue
		}

		if b.Value.Kind == ExprArray {
			return forkArray
		}

		if b.Value.Kind == ExprMap {
			return forkMap
		}

		if t, ok := p.exprType(pl, b.Value); ok && !t.Scalar() {
			return modeOf(t)
		}
	}

	return forkNone
}

// exprType is the type of the ref e in pl, or false if e does not resolve.
func (p *Program) exprType(pl *Pipeline, e *Expr) (Type, bool) {
	if e.Kind != ExprRef {
		return Type{}, false
	}

	t, rest, ok := p.refRoot(pl, e)
	if !ok {
		return Type{}, false
	}

	for _, field := range rest {
		fields, ok := p.fields(t.Base)

		i := paramIndex(fields, field)
		if !ok || i < 0 {
			return Type{}, false
		}

		if t, ok = t.project(fields[i].Type); !ok {
			return Type{}, false
		}
	}

	return t, true
}
