package gen

import (
	"slices"
	"strconv"
)

// shape is what is statically known about a collection value: its length,
// or its keys for a map. A split may zip two sources only when their fork
// counts agree, and a literal-backed source knows its shape.
type shape struct {
	known bool
	isMap bool
	n     int
	keys  []string
}

func (s shape) group() string {
	if !s.known {
		return ""
	}

	if !s.isMap {
		return "lit:arr:" + strconv.Itoa(s.n)
	}

	b := []byte("lit:map:")
	for _, k := range slices.Sorted(slices.Values(s.keys)) {
		b = strconv.AppendQuote(b, k)
	}

	return string(b)
}

// literal draws a literal of type t, sometimes null.
func (g *generator) literal(t Type) *Expr {
	if g.chance(pctNullLiteral) {
		return null()
	}

	return g.value(t, shape{})
}

// value draws a non-null literal of type t whose outer collection, when t is
// one, has shape sh if known, else a random one (often empty).
func (g *generator) value(t Type, sh shape) *Expr {
	switch {
	case t.Arr > 0:
		n := sh.n
		if !sh.known {
			n = g.collLen()
		}

		e := &Expr{Kind: ExprArray, Elems: []*Expr{}}
		for range n {
			e.Elems = append(e.Elems, g.literal(t.Elem()))
		}

		return e
	case t.Map > 0:
		keys := sh.keys
		if !sh.known {
			keys = g.drawKeys(g.collLen(), t)
		}

		e := &Expr{Kind: ExprMap, Keys: slices.Clone(keys), Elems: []*Expr{}}
		for range keys {
			e.Elems = append(e.Elems, g.literal(t.Elem()))
		}

		return e
	default:
		return g.scalar(t.Base)
	}
}

func (g *generator) scalar(base string) *Expr {
	switch base {
	case typeInt:
		return &Expr{Kind: ExprInt, Text: g.drawToken(intTokens())}
	case typeFloat:
		return &Expr{Kind: ExprFloat, Text: g.drawToken(floatTokens())}
	case typeString:
		return &Expr{Kind: ExprString, Text: g.drawToken(stringValues())}
	case typeBool:
		return boolLit(g.chance(pctTrue))
	}

	for _, s := range g.prog.Structs {
		if s.Name != base {
			continue
		}

		e := &Expr{Kind: ExprStruct}
		for _, f := range s.Fields {
			e.Keys = append(e.Keys, f.Name)
			e.Elems = append(e.Elems, g.literal(f.Type))
		}

		return e
	}

	// Files and directories have no literal mrp can resolve from the
	// pipestance, and a callable's output struct is bound by ref.
	return null()
}

func boolLit(v bool) *Expr { return &Expr{Kind: ExprBool, Text: strconv.FormatBool(v)} }

func (g *generator) drawToken(corpus []string) string { return corpus[g.index(len(corpus))] }

func (g *generator) collLen() int {
	if g.chance(pctEmpty) {
		return 0
	}

	return g.between(1, maxCollLen)
}

// drawKeys draws n distinct keys for a map of type t from the corpus. A map
// that can hold files gets no empty key, which Martian rejects as a file name.
func (g *generator) drawKeys(n int, t Type) []string {
	corpus := mapKeys()
	out := make([]string, 0, n)

	for _, i := range g.r.Perm(len(corpus)) {
		if len(out) == n {
			break
		}

		if corpus[i] != "" || !g.prog.hasFiles(t) {
			out = append(out, corpus[i])
		}
	}

	return out
}

// shapeOf is the shape of a collection literal.
func shapeOf(e *Expr) shape {
	switch e.Kind {
	case ExprArray:
		return shape{known: true, n: len(e.Elems)}
	case ExprMap:
		return shape{known: true, isMap: true, n: len(e.Elems), keys: slices.Clone(e.Keys)}
	default:
		return shape{}
	}
}
