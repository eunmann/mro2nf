package cg

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"
	"github.com/martian-lang/martian/martian/util"
)

// defaultFork is the id of an unforked node and of every fork whose index is
// zero in each part (core/fork.go:982).
const defaultFork = "fork0"

// ID is f's fork id as mrp names the fork's directory and jobs
// (core/fork.go:1010 ForkId.ForkIdString). lens[i] is the number of forks of
// f[i]'s root under the parts before it, the length its part's range has
// (ForkSourcePart.GetRange).
func (f Fork) ID(lens []int) string {
	switch len(f) {
	case 0:
		return defaultFork
	case 1:
		return partID(f[0])
	}

	var b strings.Builder
	if f.idFrom(&b, lens, 0) {
		return defaultFork
	}

	return b.String()
}

// partID is a one-part fork's id (core/fork.go:1029
// ForkSourcePart.ForkIdString).
func partID(p Part) string {
	if p.Index.Mode() == syntax.ModeMapCall {
		return "fork_" + url.PathEscape(p.Index.MapKey())
	}

	if i := p.Index.ArrayIndex(); i > 0 {
		return "fork" + strconv.Itoa(i)
	}

	return defaultFork
}

// idFrom writes the id of f's parts from start, as core's ForkId.forkId
// does (core/fork.go:1110): array parts of known length fold into one
// mixed-radix index, a variable-length one starts a new index after '_',
// a map part writes its key and a '/' before the parts after it, and an
// empty part ends the id with the index of the parts before it. It
// reports a default id, which it leaves unwritten.
func (f Fork) idFrom(b *strings.Builder, lens []int, start int) bool {
	index, dim := 0, 1

	for i, p := range f[start:] {
		alen := lens[start+i]
		if alen == 0 {
			break
		}

		if p.Index.Mode() == syntax.ModeMapCall {
			return f.mapIDFrom(b, lens, start, i, index, dim)
		}

		if alen != p.Root.Split().Source.ArrayLength() && alen > 1 && i != 0 {
			writeForkIndex(b, dim, index)
			b.WriteByte('_')

			index, dim = 0, 1
		}

		index += dim * p.Index.ArrayIndex()
		dim *= alen
	}

	if index == 0 && b.Len() == 0 {
		return true
	}

	writeForkIndex(b, dim, index)

	return false
}

// mapIDFrom writes the id from the map part at start+i. A map part after
// the first flushes the index so far, then writes the id from itself.
func (f Fork) mapIDFrom(b *strings.Builder, lens []int, start, i, index, dim int) bool {
	if i != 0 {
		writeForkIndex(b, dim, index)
		b.WriteByte('/')

		return f.idFrom(b, lens, start+i)
	}

	b.WriteString("fork_" + url.PathEscape(f[start].Index.MapKey()))

	if start < len(f)-1 {
		b.WriteByte('/')

		if f.idFrom(b, lens, start+1) {
			b.WriteString(defaultFork)
		}
	}

	return false
}

// writeForkIndex writes a mixed-radix fork index, zero-padded to the width
// of its largest value (core/fork.go:1205 writeForkIndex,
// writePaddedIndex).
func writeForkIndex(b *strings.Builder, dim, index int) {
	if dim < 10 && index == 0 {
		b.WriteString(defaultFork)

		return
	}

	id := strconv.Itoa(index)

	b.WriteString("fork")
	b.WriteString(strings.Repeat("0", max(0, util.WidthForInt(dim-1)-len(id))))
	b.WriteString(id)
}
