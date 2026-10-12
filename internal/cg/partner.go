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

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/martian-lang/martian/martian/syntax"
)

// AbsentName names, among a partner's element dirs, the record of the
// indices it has no element at; no index token (IndexToken) is one.
const AbsentName = "none"

var errPartnerSources = errors.New("splits of one output over one root disagree")

// partner is a split over a fork root of an output of a stage that does
// not fix the root's forks: a map call zipping the outputs of different
// stages. The stage's post writes the output's elements one dir per index,
// and an instance stages only the one at its own index (zip_runtime).
type partner struct {
	root  *syntax.CallGraphStage
	split *syntax.SplitExp
	ref   *syntax.RefExp
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
// stage output, of a stage other than the one that fixes the root's forks,
// and forked on no root st is not. A partner stage forked on such a root
// gathers there, which no one element serves.
func (g *Graph) partnerOf(st *syntax.CallGraphStage, sp *syntax.SplitExp) (partner, bool) {
	ref, ok := sp.Value.(*syntax.RefExp)
	if !ok || ref.Kind == syntax.KindSelf {
		return partner{}, false
	}

	i := slices.IndexFunc(st.ForkRoots(), func(r *syntax.CallGraphStage) bool { return r.Call() == sp.Call })
	if i < 0 {
		return partner{}, false
	}

	root := st.ForkRoots()[i]
	if prod, fed := g.Producer(root); fed && prod == ref.Id {
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

// partners are the partners of node's outputs over every root, one per
// root and output, in dir order.
func (g *Graph) partners(node string) ([]partner, error) {
	found := map[string]partner{}

	var errs []error

	for _, fqid := range slices.Sorted(maps.Keys(g.stages)) {
		st := g.stages[fqid]

		for _, k := range slices.Sorted(maps.Keys(st.ResolvedInputs())) {
			walkSplits(st.ResolvedInputs()[k].Exp, func(sp *syntax.SplitExp) {
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
// partner reads: one dir per index of the output, named by its IndexToken,
// and the absent record.
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

	raw, err := marshal(v)
	if err != nil {
		return err
	}

	idx, err := indicesOf(raw, p.root.Split().Source.CallMode())
	if err != nil {
		return err
	}

	param, err := collectionParam(elementField, p.split, p.root)
	if err != nil {
		return err
	}

	dir := filepath.Join(w.dir, p.dir())
	rec := partnerJSON{Root: p.root.Fqid, Node: p.ref.Id, Output: p.ref.OutputId}

	for _, ix := range idx {
		el, err := element(v, ix)
		if err != nil {
			return err
		}

		ed := filepath.Join(dir, IndexToken(ix))
		if err := writeElementValue(ed, map[string]json.Marshaler{elementField: el}, []ir.Param{param}, w.tbl); err != nil {
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

		if err := writeRecord(filepath.Join(ed, partnerFile), at); err != nil {
			return err
		}
	}

	rec.Null = isNull(raw)

	return writeRecord(filepath.Join(dir, AbsentName, partnerFile), rec)
}

// addPartner indexes a staged partner dir: an element, by its index, or the
// absent record of its output.
func (s *stagedStore) addPartner(dir string) error {
	var rec partnerJSON
	if err := readRecord(filepath.Join(dir, partnerFile), &rec); err != nil {
		return err
	}

	k := partnerKey(rec.Root, rec.Node, rec.Output)

	switch {
	case rec.Index != nil && rec.Key == nil:
		s.partners[k+"\x00"+IndexToken(ArrayIndex(*rec.Index))] = dir
	case rec.Key != nil && rec.Index == nil:
		s.partners[k+"\x00"+IndexToken(MapKey(*rec.Key))] = dir
	case rec.Key == nil && rec.Index == nil:
		s.absent[k] = &rec.Null
	default:
		return fmt.Errorf("partner element %s: %w: both an index and a key", dir, errBadFork)
	}

	return nil
}

// partner serves a split over root of node's output at ix from the
// partner element staged for it: its value, null where the output is null,
// or errNoElement where the output has no element at ix. It is false when
// nothing for the output is staged.
func (s *stagedStore) partner(root *syntax.CallGraphStage, ref *syntax.RefExp, ix syntax.CollectionIndex) (json.RawMessage, bool, error) {
	k := partnerKey(root.Fqid, ref.Id, ref.OutputId)

	if dir, ok := s.partners[k+"\x00"+IndexToken(ix)]; ok {
		raw, err := readElementField(dir)

		return raw, true, err
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

// readElementField reads the one value of a partner element's bundle.
func readElementField(dir string) (json.RawMessage, error) {
	raw, err := shim.ReadBundle(dir)
	if err != nil {
		return nil, fmt.Errorf("partner element %s: %w", dir, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode partner element %s: %w", dir, err)
	}

	return payload[elementField], nil
}
