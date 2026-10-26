package cg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/martian-lang/martian/martian/syntax"
)

// AbsentName names, among a partner's element dirs, the record of the
// indices it has no element at; no index token (IndexToken) is one.
const AbsentName = "none"

var errPartnerSources = errors.New("splits of one output over one root disagree")

// partner is a split over a fork root of a stage output other than the one
// the root's forks come from: a map call zipping several outputs, of one
// stage or of several, or a disable binding splitting one. The stage's post
// writes the output's elements one per index, and an instance gets only
// the one at its own index (zip_runtime). A stage a runtime gate
// disables is read through the gate, `split Q.o unless G.f`: an instance
// reads the gate, and the element only where the gate holds Q enabled
// (#494).
type partner struct {
	root  *syntax.CallGraphStage
	split *syntax.SplitExp
	ref   *syntax.RefExp
}

// partnerSource is the stage output a split reads, and the gate it reads
// it through: `split Q.o`, or `split Q.o unless G.f`.
func partnerSource(sp *syntax.SplitExp) (*syntax.RefExp, *syntax.RefExp, bool) {
	switch v := sp.Value.(type) {
	case *syntax.RefExp:
		return v, nil, true
	case *syntax.DisabledExp:
		ref, rok := v.Value.(*syntax.RefExp)
		gate, gok := v.Disabled.(*syntax.RefExp)

		return ref, gate, rok && gok
	default:
		return nil, nil, false
	}
}

// PartnerDir names the dir of root's partner elements of node's output, in
// node's posted forks.
func PartnerDir(root, node, output string) string { return root + "@" + node + "." + output }

func (p partner) dir() string { return PartnerDir(p.root.Fqid, p.ref.Id, p.ref.OutputId) }

// IndexToken names a fork index alike in a partner's element dirs and in
// its consumers' forks: an array position in decimal, or the hex SHA-256 of
// a map key, which no file name limit or character in the key breaks.
func IndexToken(ix syntax.CollectionIndex) string {
	if ix.Mode() == syntax.ModeMapCall {
		sum := sha256.Sum256([]byte(ix.MapKey()))

		return hex.EncodeToString(sum[:])
	}

	return strconv.Itoa(ix.ArrayIndex())
}

// partnerOf is the partner sp is in st: a split over one of st's roots of a
// stage output other than the root's own source, of a stage forked on no
// root st is not. A partner stage forked on such a root gathers there,
// which no one element serves.
func (g *Graph) partnerOf(st *syntax.CallGraphStage, sp *syntax.SplitExp) (partner, bool) {
	ref, gate, ok := partnerSource(sp)
	if !ok || ref.Kind == syntax.KindSelf || gate != nil && gate.Kind == syntax.KindSelf {
		return partner{}, false
	}

	i := slices.IndexFunc(st.ForkRoots(), func(r *syntax.CallGraphStage) bool { return r.Call() == sp.Call })
	if i < 0 {
		return partner{}, false
	}

	root := st.ForkRoots()[i]
	if g.ownSource(root, ref) {
		return partner{}, false
	}

	up, err := g.Stage(ref.Id)
	if err != nil {
		return partner{}, false
	}

	for _, r := range up.ForkRoots() {
		if r.Fqid == root.Fqid || !slices.ContainsFunc(st.ForkRoots(), func(x *syntax.CallGraphStage) bool { return x.Fqid == r.Fqid }) {
			return partner{}, false
		}
	}

	return partner{root: root, split: sp, ref: ref}, true
}

// ownSource reports whether ref reads the stage output root's forks come
// from, whose element root's fork element carries. A root a literal's
// known length forks is fed by no stage, whichever split is its Value.
func (g *Graph) ownSource(root *syntax.CallGraphStage, ref *syntax.RefExp) bool {
	if prod, fed := g.Producer(root); !fed || prod != ref.Id {
		return false
	}

	own, ok := root.Split().Value.(*syntax.RefExp)

	return ok && own.OutputId == ref.OutputId
}

// partners are the partners of node's outputs over every root, one per
// root and output, in dir order.
func (g *Graph) partners(node string) ([]partner, error) {
	found := map[string]partner{}

	var errs []error

	for _, fqid := range slices.Sorted(maps.Keys(g.stages)) {
		st := g.stages[fqid]

		for _, e := range bindings(st) {
			walkSplits(e, func(sp *syntax.SplitExp) {
				p, ok := g.partnerOf(st, sp)
				if !ok || p.ref.Id != node {
					return
				}

				if was, ok := found[p.dir()]; ok && !sameRef(was.ref, p.ref) {
					errs = append(errs, fmt.Errorf("%w: %s.%s over %s", errPartnerSources, p.ref.Id, p.ref.OutputId, p.root.Fqid))
				}

				found[p.dir()] = p
			})
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	out := make([]partner, 0, len(found))
	for _, d := range slices.Sorted(maps.Keys(found)) {
		out = append(out, found[d])
	}

	return out, nil
}

// partnerJSON is a partner dir's record: an element at an index or key,
// or, with neither, the absent record, which tells whether the output is
// null, so that a split reads null at every index, or holds no element at
// the consumer's index.
type partnerJSON struct {
	Root   string  `json:"root"`
	Node   string  `json:"node"`
	Output string  `json:"output"`
	Index  *int    `json:"index,omitempty"`
	Key    *string `json:"key,omitempty"`
	Null   bool    `json:"null,omitempty"`
}

func partnerKey(root, node, output string) string { return root + "\x00" + node + "\x00" + output }

// writePartners writes the posting instance's elements of each output a
// partner reads: one per index of the output, named by its IndexToken
// (item), and the absent record, which holds no value, as a value record.
func (w forkWriter) writePartners(node string, fork Fork) error {
	ps, err := w.r.g.partners(node)
	if err != nil {
		return err
	}

	for _, p := range ps {
		if err := w.partner(p, fork); err != nil {
			return fmt.Errorf("elements of %s.%s over %s: %w", p.ref.Id, p.ref.OutputId, p.root.Fqid, err)
		}
	}

	return nil
}

func (w forkWriter) partner(p partner, fork Fork) error {
	v, err := w.r.resolveRef(p.ref, nil, fork)
	if err != nil {
		return err
	}

	c, err := indexedOf(v, p.root.Split().Source.CallMode())
	if err != nil {
		return err
	}

	param, err := w.r.g.collectionParam(elementField, p.split, p.root)
	if err != nil {
		return err
	}

	rec := partnerJSON{Root: p.root.Fqid, Node: p.ref.Id, Output: p.ref.OutputId}

	for _, ix := range c.indices {
		el, err := c.at(ix)
		if err != nil {
			return err
		}

		at := rec
		if ix.Mode() == syntax.ModeMapCall {
			k := ix.MapKey()
			at.Key = &k
		} else {
			n := ix.ArrayIndex()
			at.Index = &n
		}

		if err := w.item(p.dir(), IndexToken(ix), el, param, valueJSON{Partner: &at}); err != nil {
			return err
		}
	}

	rec.Null = c.null

	return w.record(p.dir()+"/"+AbsentName, valueJSON{Partner: &rec})
}

// addPartner indexes a staged partner dir.
func (s *stagedStore) addPartner(dir string) error {
	var rec partnerJSON
	if err := readRecord(filepath.Join(dir, partnerFile), &rec); err != nil {
		return err
	}

	return s.indexPartner(rec, elementItem{dir: dir}, dir)
}

// indexPartner indexes the partner record rec of an element, by its index,
// whose value it holds, or the absent record of its output. name names
// where it came from.
func (s *stagedStore) indexPartner(rec partnerJSON, it elementItem, name string) error {
	k := partnerKey(rec.Root, rec.Node, rec.Output)

	switch {
	case rec.Index != nil && rec.Key == nil:
		s.partners[k+"\x00"+IndexToken(ArrayIndex(*rec.Index))] = it
	case rec.Key != nil && rec.Index == nil:
		s.partners[k+"\x00"+IndexToken(MapKey(*rec.Key))] = it
	case rec.Key == nil && rec.Index == nil:
		s.absent[k] = &rec.Null
	default:
		return fmt.Errorf("partner element %s: %w: both an index and a key", name, errBadFork)
	}

	return nil
}

// partner serves a split over root of node's output at ix from the
// partner element staged for it: its value, null where the output is null,
// or errNoElement where the output has no element at ix. It is false when
// nothing for the output is staged.
func (s *stagedStore) partner(root *syntax.CallGraphStage, ref *syntax.RefExp, ix syntax.CollectionIndex) (json.RawMessage, bool, error) {
	k := partnerKey(root.Fqid, ref.Id, ref.OutputId)

	if it, ok := s.partners[k+"\x00"+IndexToken(ix)]; ok {
		raw, held, err := it.read(s.refs)
		if err == nil && !held {
			err = errNotItem
		}

		if err != nil {
			return nil, true, fmt.Errorf("partner element of %s.%s: %w", ref.Id, ref.OutputId, err)
		}

		return raw, true, nil
	}

	null, ok := s.absent[k]

	switch {
	case !ok:
		return nil, false, nil
	case *null:
		return nil, true, nil
	default:
		return nil, true, fmt.Errorf("%w: %s.%s at %s of %s", errNoElement, ref.Id, ref.OutputId, ix.GoString(), root.Fqid)
	}
}

func (emptyStore) partner(*syntax.CallGraphStage, *syntax.RefExp, syntax.CollectionIndex) (json.RawMessage, bool, error) {
	return nil, false, nil
}
