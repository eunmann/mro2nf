// Package gen generates random, well-typed Martian programs with stub py
// stages, for the generative differential test against mrp (test/e2e
// TestGenDiff), and shrinks a program that diverges to a minimal one.
//
// Generation is a pure function of the seed: it draws from math/rand/v2 with
// an explicit PCG seed, never from the clock or the global source. A program
// is kept only when the real Martian compiler accepts it (Validate).
package gen

import (
	"fmt"
	"math/rand/v2"
	"slices"
)

// maxAttempts bounds how many candidate programs Generate draws for one seed
// before giving up. Construction is type-directed, so the compiler rejects few.
const maxAttempts = 50

// Generate returns the program for seed: the first candidate, in a fixed
// attempt order, that the Martian compiler accepts.
func Generate(seed uint64) (*Program, error) {
	var lastErr error

	for attempt := range uint64(maxAttempts) {
		p := build(seed, attempt)
		if err := Validate(p); err != nil {
			lastErr = err

			continue
		}

		return p, nil
	}

	return nil, fmt.Errorf("generate seed %d: no valid program in %d attempts: %w", seed, maxAttempts, lastErr)
}

// generator holds the state of one candidate program under construction.
type generator struct {
	r       *rand.Rand
	prog    *Program
	count   map[string]int
	failing bool
}

func build(seed, attempt uint64) *Program {
	g := &generator{
		r:     rand.New(rand.NewPCG(seed, attempt)),
		prog:  &Program{Seed: seed, Attempt: attempt},
		count: map[string]int{},
	}

	g.fileTypes()
	g.structs()

	top := g.pipeline("TOP", 0, nil, false)
	g.prog.Pipelines = append(g.prog.Pipelines, top.p)
	g.prog.Entry = top.entry

	return g.prog
}

// name returns a fresh identifier with the prefix. Names are unique across
// the program, so no two parameters, outputs or fields ever collide.
func (g *generator) name(prefix string) string {
	n := g.count[prefix]
	g.count[prefix]++

	return fmt.Sprintf("%s%d", prefix, n)
}

func (g *generator) chance(pct int) bool { return g.roll() < pct }

// roll draws from [0, percent), for a choice among bands.
func (g *generator) roll() int { return g.r.IntN(percent) }

// between returns an int in [lo, hi].
func (g *generator) between(lo, hi int) int { return lo + g.r.IntN(hi-lo+1) }

// index draws an index into a collection of n elements.
func (g *generator) index(n int) int { return g.r.IntN(n) }

func (g *generator) fileTypes() {
	corpus := fileTypes()
	order := g.r.Perm(len(corpus))

	for _, i := range order[:g.between(1, maxFileTypes)] {
		g.prog.FileTypes = append(g.prog.FileTypes, corpus[i])
	}
}

// structs declares up to maxStructs user structs. A struct's fields may hold
// earlier structs, so nesting is acyclic, and file fields are common.
func (g *generator) structs() {
	for range g.between(0, maxStructs) {
		s := &Struct{Name: g.name("S")}

		for range g.between(1, maxStructFields) {
			s.Fields = append(s.Fields, Param{Name: g.name("f"), Type: g.randType(true)})
		}

		g.prog.Structs = append(g.prog.Structs, s)
	}
}

func (g *generator) fileBase() string {
	if g.chance(pctBuiltinFile) {
		return typeFile
	}

	return g.prog.FileTypes[g.index(len(g.prog.FileTypes))]
}

func (g *generator) randBase() string {
	roll := g.roll()

	switch {
	case roll < intBelow:
		return typeInt
	case roll < floatBelow:
		return typeFloat
	case roll < stringBelow:
		return typeString
	case roll < boolBelow:
		return typeBool
	case roll < fileBelow:
		return g.fileBase()
	case roll < pathBelow:
		return typePath
	case len(g.prog.Structs) > 0:
		return g.prog.Structs[g.index(len(g.prog.Structs))].Name
	default:
		return typeInt
	}
}

// randType draws a type: mostly scalars, then arrays, 2-D arrays and typed
// maps (map<T>, map<T[]> and map<T>[]). allowMap false keeps maps out, for an
// output a per-key map call would nest in a second map.
func (g *generator) randType(allowMap bool) Type {
	t := Type{Base: g.randBase()}
	roll := g.roll()

	switch {
	case roll < scalarBelow:
	case roll < arrayBelow:
		t.Arr = 1
	case roll < array2DBelow:
		t.Arr = 2
	case !allowMap:
		t.Arr = 1
	case roll < mapBelow:
		t.Map = 1
	case roll < mapOfArrsBelow:
		t.Map = 2
	default:
		t.Map, t.Arr = 1, 1
	}

	return t
}

// hasFiles reports whether a value of type t can hold a file or directory.
// Martian requires the keys of such a map to be legal file names, since mrp
// publishes each key as a directory.
func (p *Program) hasFiles(t Type) bool {
	return p.baseHasFiles(t.Base, map[string]bool{})
}

func (p *Program) baseHasFiles(base string, seen map[string]bool) bool {
	if base == typePath || p.isFileBase(base) {
		return true
	}

	if seen[base] {
		return false
	}

	seen[base] = true

	fields, _ := p.fields(base)
	for _, f := range fields {
		if p.baseHasFiles(f.Type.Base, seen) {
			return true
		}
	}

	return false
}

// isFileBase reports whether base is a file type: the builtin file or a
// declared filetype.
func (p *Program) isFileBase(base string) bool {
	return base == typeFile || slices.Contains(p.FileTypes, base)
}
