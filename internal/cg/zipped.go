package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/martian-lang/martian/martian/syntax"
)

var errZipSources = errors.New("splits of one output disagree on its forks")

// zipSource is a split over a fork root of another output of the stage
// that feeds the root: a map call zipping two of its outputs, or a disable
// binding splitting one. The root's fork element carries its element too,
// as member, so a fork stages none of the output's other elements.
type zipSource struct {
	member string
	split  *syntax.SplitExp
	ref    *syntax.RefExp
}

// zipMember is the fork element member that holds the element of the
// producer's output id.
func zipMember(id string) string { return "out:" + id }

// zipSources are the splits over root, other than its own source, of the
// outputs of the stage that feeds it, one per output, in output order.
func (g *Graph) zipSources(root *syntax.CallGraphStage) ([]zipSource, error) {
	prod, ok := g.Producer(root)
	if !ok {
		return nil, nil
	}

	own, _ := root.Split().Value.(*syntax.RefExp)
	found := map[string]zipSource{}

	var errs []error

	visit := func(sp *syntax.SplitExp) {
		ref, ok := sp.Value.(*syntax.RefExp)
		if !ok || sp.Call != root.Call() || ref.Id != prod || (own != nil && ref.OutputId == own.OutputId) {
			return
		}

		if was, ok := found[ref.OutputId]; ok && !sameRef(was.ref, ref) {
			errs = append(errs, fmt.Errorf("%w: %s.%s over %s", errZipSources, ref.Id, ref.OutputId, root.Fqid))
		}

		found[ref.OutputId] = zipSource{member: zipMember(ref.OutputId), split: sp, ref: ref}
	}

	for _, st := range g.stages {
		for _, rb := range st.ResolvedInputs() {
			walkSplits(rb.Exp, visit)
		}

		for _, d := range st.Disabled() {
			walkSplits(d, visit)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	out := make([]zipSource, 0, len(found))
	for _, id := range slices.Sorted(maps.Keys(found)) {
		out = append(out, found[id])
	}

	return out, nil
}

// walkSplits calls visit on every split in e.
func walkSplits(e syntax.Exp, visit func(*syntax.SplitExp)) {
	switch x := e.(type) {
	case *syntax.SplitExp:
		visit(x)
		walkSplits(x.Value, visit)
	case *syntax.ArrayExp:
		for _, v := range x.Value {
			walkSplits(v, visit)
		}
	case *syntax.MapExp:
		for _, v := range x.Value {
			walkSplits(v, visit)
		}
	case *syntax.MergeExp:
		walkSplits(x.Value, visit)
	case *syntax.DisabledExp:
		walkSplits(x.Disabled, visit)
		walkSplits(x.Value, visit)
	}
}

// zipped is the collections of a root's zipped sources under an outer fork,
// each with the param its elements are written with.
type zipped struct {
	srcs   []zipSource
	values []json.Marshaler
	params []*syntax.StructMember
}

// zippedAt resolves each zipped source of root under outer, as resolveRef
// reads the producer's output.
func (w forkWriter) zippedAt(root *syntax.CallGraphStage, outer Fork) (zipped, error) {
	srcs, err := w.r.g.zipSources(root)
	if err != nil {
		return zipped{}, err
	}

	z := zipped{srcs: srcs}

	for _, s := range srcs {
		v, err := w.r.resolveRef(s.ref, nil, outer)
		if err != nil {
			return zipped{}, fmt.Errorf("zipped source of %s: %w", root.Fqid, err)
		}

		p, err := w.r.g.collectionParam(s.member, s.split, root)
		if err != nil {
			return zipped{}, err
		}

		z.values, z.params = append(z.values, v), append(z.params, p)
	}

	return z, nil
}

// at adds to payload each zipped source's element at ix, and returns the
// members whose source has none there.
func (z zipped) at(ix syntax.CollectionIndex, payload map[string]json.Marshaler) ([]string, error) {
	var missing []string

	for i, s := range z.srcs {
		el, err := element(z.values[i], ix)

		switch {
		case errors.Is(err, errNoElement):
			missing = append(missing, s.member)
		case err != nil:
			return nil, fmt.Errorf("element of %s: %w", s.member, err)
		default:
			payload[s.member] = el
		}
	}

	return missing, nil
}
