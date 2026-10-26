// Copied from Martian core (martian-lang/martian 50ad6c0, MIT: see
// LICENSE.martian) and adapted to run over one task's staged values; each
// adaptation is declared in test/thirdparty (mrcoreAdapted).

package mrcore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/martian-lang/martian/martian/syntax"
)

var nullBytes = []byte(syntax.KindNull)

var missingValueError = ErrNoElement

// TopNode is core's TopNode (pipestance.go:951) over the call graph of one
// invocation, its outs read from a Store.
type TopNode struct {
	types    *syntax.TypeLookup
	allNodes map[string]*Node
	store    Store
	// unstaged are the outs reads that found nothing staged, which core
	// reads as a node not yet ready.
	unstaged []error
	// collections are the split collections read from stage outputs,
	// decoded once (refElement).
	collections map[collectionKey]json.Marshaler
}

// Node is core's Node (node.go:55): a call graph node and its forks, built
// on first use and, scoped to a fork, in views.
type Node struct {
	top       *TopNode
	call      syntax.CallGraphNode
	forkRoots []*syntax.CallStm
	forkIds   ForkIdSet
	forks     []*Fork
	built     bool
	expandErr error
	views     map[string]*Node
}

// Fork is core's Fork (stage.go:356): one fork of a node, in core's
// DisabledState where its expansion found no forks (Fork.writeDisable).
type Fork struct {
	node          *Node
	forkId        ForkId
	fqname        string
	disabledState bool
}

// resolveInputs is core's (resolve.go:61) as the _args of a job resolve
// them (stage.go:1402, keepSplit false), with no read size limit.
func (node *Node) resolveInputs(fork ForkId) (MarshalerMap, error) {
	const readSize = 0
	result := make(MarshalerMap, len(node.call.ResolvedInputs()))
	var errs syntax.ErrorList
	for k, v := range node.call.ResolvedInputs() {
		ready, r, err := node.top.resolve(v.Exp, v.Type, fork, readSize)
		if err == nil && !ready {
			err = node.top.notReady()
		}
		if err != nil {
			errs = append(errs, &elementError{
				element: "parameter " + k,
				inner:   err,
			})
		}
		result[k] = r
	}
	return result, errs.If()
}

func (node *Node) resolvePipelineOutputs(fork ForkId) (json.Marshaler, syntax.Type, error) {
	const readSize = 0
	ro := node.call.ResolvedOutputs()
	ready, r, err := node.top.resolve(ro.Exp, ro.Type, fork, readSize)
	if err == nil && !ready {
		err = node.top.notReady()
	}
	if err != nil {
		tid := ro.Type.TypeId()
		err = &elementError{
			element: node.GetFQName() + " fork " + fork.GoString() + " (" + tid.String() + ")",
			inner:   err,
		}
	}
	return r, ro.Type, err
}

func (node *TopNode) resolveMerge(binding *syntax.MergeExp, t syntax.Type,
	fork ForkId, readSize int64) (bool, json.Marshaler, error) {
	var innerT syntax.Type
	switch t := t.(type) {
	case *syntax.ArrayType:
		if binding.MergeOver.CallMode() != syntax.ModeArrayCall {
			return false, nil, fmt.Errorf(
				"expected array but mapping is %s",
				binding.MergeOver.CallMode().String())
		}
		if t.Dim > 1 {
			nt := *t
			nt.Dim--
			innerT = &nt
		} else {
			innerT = t.Elem
		}
	case *syntax.TypedMapType:
		if binding.MergeOver.CallMode() != syntax.ModeMapCall {
			tid := t.TypeId()
			return false, nil, fmt.Errorf(
				"expected %s but mapping is %s",
				tid.String(),
				binding.MergeOver.CallMode().String())
		}
		innerT = t.Elem
	default:
		tid := t.TypeId()
		panic("invalid type for " + binding.GoString() +
			" for " + binding.Call.GetFqid() + ": " + tid.String())
	}
	for _, part := range fork {
		if part.Split.Call == binding.GetCall() {
			return node.resolve(binding.Value, innerT, fork, readSize)
		}
	}
	var forkRefId string
	constraint := binding.ForkNode
	if constraint != nil {
		forkRefId = constraint.Id
	} else {
		forkRefId = binding.Call.GetFqid()
		if ref, ok := binding.Value.(*syntax.RefExp); ok && ref.Id == forkRefId {
			constraint = ref
		}
	}
	partsFork := fork
	if constraint != nil {
		// The merge may have been statically constrained to a subset of the
		// forks of the bound node, for example if this is a merge inside one
		// instance of a statically mapped pipeline.
		if boundNode := node.allNodes[forkRefId]; boundNode != nil {
			partsFork = fork.constrain(constraint.Forks, boundNode.forkRoots)
		}
	}
	parts, errs := node.getParts(binding.GetCall(), partsFork, forkRefId)
	allReady := true
	switch binding.CallMode() {
	case syntax.ModeMapCall:
		if len(parts) == 1 {
			if _, ok := parts[0].Id.(emptyFork); ok {
				err := errs.If()
				if err != nil {
					err = &elementError{
						element: "merge of map " + binding.Call.GetFqid(),
						inner:   err,
					}
				}
				return true, MarshalerMap{}, err
			}
		}
		result := make(MarshalerMap, len(parts))
		for _, part := range parts {
			if part.Id.IndexSource() != nil {
				errs = append(errs, &elementError{
					element: "unresolved fork source for mapped " +
						part.Split.Call.GoString(),
				})
				continue
			}
			switch part.Id.Mode() {
			case syntax.ModeArrayCall:
				return true, result, fmt.Errorf("unexpected array fork")
			case syntax.ModeMapCall:
				ready, v, err := node.resolve(binding.Value,
					innerT,
					append(fork, part), readSize)
				if err != nil {
					errs = append(errs, &elementError{
						element: "key " + part.Id.GoString(),
						inner:   err,
					})
				}
				allReady = allReady && ready
				result[part.Id.MapKey()] = v
			}
		}
		err := errs.If()
		if err != nil {
			err = &elementError{
				element: "merge of map " + binding.Call.GetFqid(),
				inner:   err,
			}
		}
		return allReady, result, err
	case syntax.ModeNullMapCall:
		return true, nil, nil
	case syntax.ModeArrayCall:
		if len(parts) == 1 {
			if _, ok := parts[0].Id.(emptyFork); ok {
				err := errs.If()
				if err != nil {
					err = &elementError{
						element: "merge of array " + binding.Call.GetFqid(),
						inner:   err,
					}
				}
				return true, marshallerArray{}, err
			}
		}
		result := make(marshallerArray, len(parts))
		for i, part := range parts {
			if part.Id.IndexSource() != nil {
				errs = append(errs, &elementError{
					element: "unresolved fork for array-mapped " +
						part.Split.Call.GoString(),
				})
				continue
			}
			switch part.Id.Mode() {
			case syntax.ModeMapCall:
				return true, result, fmt.Errorf("unexpected map fork")
			case syntax.ModeArrayCall:
				ready, v, err := node.resolve(binding.Value,
					innerT,
					append(fork, part), readSize)
				if err != nil {
					errs = append(errs, &elementError{
						element: "index " + part.Id.GoString(),
						inner:   err,
					})
				}
				allReady = allReady && ready
				result[i] = v
			}
		}
		err := errs.If()
		if err != nil {
			err = &elementError{
				element: "merge of array " + binding.Call.GetFqid(),
				inner:   err,
			}
		}
		return allReady, result, err
	}
	panic("invalid mapping mode")
}

// getParts returns the ForkSourcePart corresponding the the given call for
// every fork of the given node which matches the given fork ID.
func (node *TopNode) getParts(src *syntax.CallStm,
	forkId ForkId,
	id string) ([]*ForkSourcePart, syntax.ErrorList) {
	boundNode := node.allNodes[id]
	if boundNode == nil {
		panic("unknown bound node - this should not be possible in properly-compiled code")
	}
	parts := boundNode.tableParts(src)
	boundNode = boundNode.view(forkId)
	var errs syntax.ErrorList
	if boundNode.expandErr != nil {
		errs = append(errs, boundNode.expandErr)
	}
	if len(parts) == 1 && parts[0].Id.IndexSource() != nil &&
		(parts[0].Range == nil || parts[0].Range.Length() >= 0) {
		matchingParts := make([]*ForkSourcePart, 0, len(boundNode.forks))
		for _, fork := range boundNode.forks {
			if p, err := fork.forkId.matchPart(parts[0].Split.Call); err != nil {
				if parts[0].Split.Call == src {
					errs = append(errs, &forkResolutionError{
						Msg: "circular fork sources",
					})
				} else {
					errs = append(errs, &elementError{
						element: "unmatched call " + parts[0].Split.Call.GoString(),
						inner:   err,
					})
				}
			} else if fork.forkId.Matches(forkId) {
				matchingParts = append(matchingParts, p)
			}
		}
		parts = matchingParts
	} else if len(parts) == 0 && src.KnownLength() {
		switch src.CallMode() {
		case syntax.ModeArrayCall:
			if src.ArrayLength() > 0 {
				parts = make([]*ForkSourcePart, src.ArrayLength())
				partStore := make([]ForkSourcePart, src.ArrayLength())
				for i := range parts {
					partStore[i] = ForkSourcePart{
						Split: &syntax.SplitExp{
							Value:  &syntax.MergeExp{MergeOver: src},
							Source: src,
							Call:   src,
						},
						Id: arrayIndexFork(i),
					}
					parts[i] = &partStore[i]
				}
			}
		case syntax.ModeMapCall:
			if keyMap := src.Keys(); len(keyMap) > 0 {
				keys := make([]string, 0, len(keyMap))
				for k := range keyMap {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				partStore := make([]ForkSourcePart, len(keys))
				parts = make([]*ForkSourcePart, len(keys))
				for i, k := range keys {
					partStore[i] = ForkSourcePart{
						Split: &syntax.SplitExp{
							Value:  &syntax.MergeExp{MergeOver: src},
							Source: src,
							Call:   src,
						},
						Id: mapKeyFork(k),
					}
					parts[i] = &partStore[i]
				}
			}
		}
	} else if len(boundNode.forks[0].forkId) > 1 {
		id := make(ForkId, len(forkId), len(forkId)+1)
		copy(id, forkId)
		allow := func(part *ForkSourcePart) bool {
			for _, fork := range boundNode.forks {
				if fork.forkId.Matches(forkId) {
					if p, err := fork.forkId.matchPart(part.Split.Call); err != nil {
						errs = append(errs, &elementError{
							element: "unmatched fork source " + part.Split.Call.GoString(),
							inner:   err,
						})
					} else if indexEqual(p.Id, part.Id) {
						return true
					}
				}
			}
			return false
		}
		for len(parts) > 0 && !allow(parts[0]) {
			parts = parts[1:]
		}
		for len(parts) > 1 && !allow(parts[len(parts)-1]) {
			parts = parts[:len(parts)-1]
		}
		alloc := false
		for i := len(parts) - 1; i > 1; i-- {
			if !alloc {
				parts = append(parts[:i:i], parts[i+1:]...)
				alloc = true
			} else {
				parts = append(parts[:i], parts[i+1:]...)
			}
		}
	} else if len(parts) == 0 {
		return forkId.getUnmatchedForkParts(boundNode), errs
	}
	return parts, errs
}

func (node *TopNode) resolveSplit(binding *syntax.SplitExp, t syntax.Type,
	fork ForkId, readSize int64) (bool, json.Marshaler, error) {
	// If the split is due to a common ancesstor pipeline splitting, use
	// that.
	var innerT syntax.Type
	switch binding.Source.CallMode() {
	case syntax.ModeArrayCall:
		innerT = node.types.GetArray(t, 1)
	case syntax.ModeMapCall:
		innerT = node.types.GetMap(t)
	}
	for i, part := range fork {
		if part.Split.Call == binding.Call || i == len(fork)-1 {
			if r, done := node.splitElement(binding, t, fork, part, readSize); done {
				return r.ready, r.v, r.err
			}
			ready, result, err := node.resolve(
				binding.Value,
				innerT, fork, readSize)
			if err != nil {
				return ready, result, &elementError{
					element: "split " + binding.Source.CallMode().String(),
					inner:   err,
				}
			}
			if !ready {
				return ready, result, nil
			}
			if ref, ok := binding.Value.(*syntax.RefExp); ok {
				// RefExp resolution has already done the element extraction.
				for c := range ref.Forks {
					if c == binding.Call {
						return ready, result, nil
					}
				}
			}
			e, err := getElement(result, part.Id)
			if err != nil {
				err = &elementError{
					element: "splitting " + binding.Source.CallMode().String() +
						" " + binding.Value.GoString() + " with fork part " +
						part.Split.Call.GoString(),
					inner: err,
				}
			}
			return ready, e, err
		}
	}
	// This fork's split is unresolved.
	ready, result, err := node.resolve(
		binding.Value,
		innerT, fork, readSize)
	if err != nil {
		return ready, result, &elementError{
			element: "self-split " + binding.Source.CallMode().String(),
			inner:   err,
		}
	}
	if !ready {
		return ready, result, nil
	}
	if result != binding.Value {
		var r syntax.Exp
		switch rr := result.(type) {
		case syntax.Exp:
			r = rr
		default:
			var parser syntax.Parser
			r, err = convertToExp(&parser, false, result, innerT.TypeId(), node.types)
			if err != nil {
				return ready, binding, &elementError{
					element: "unresolved split value",
					inner:   err,
				}
			}
		}
		b := *binding
		b.Value = r
		return ready, &b, nil
	}
	return ready, binding, nil
}

func getArrayElement(array json.Marshaler, i int) (json.Marshaler, error) {
	if i < 0 {
		return nil, &elementError{
			element: "array index " + strconv.Itoa(i),
		}
	}
	switch array := array.(type) {
	case *syntax.NullExp:
		return array, nil
	case json.RawMessage:
		var r marshallerArray
		if err := r.UnmarshalJSON(array); err != nil {
			return array, &elementError{
				element: "array index " + strconv.Itoa(i),
				inner:   err,
			}
		}
		if i >= len(r) {
			return nil, &elementError{
				element: "array index " + strconv.Itoa(i) +
					" >= " + strconv.Itoa(len(r)),
				inner: ErrNoElement,
			}
		}
		return r[i], nil
	case marshallerArray:
		if i >= len(array) {
			return nil, &elementError{
				element: "array index " + strconv.Itoa(i) +
					" >= " + strconv.Itoa(len(array)),
				inner: ErrNoElement,
			}
		}
		return array[i], nil
	case *syntax.ArrayExp:
		if i >= len(array.Value) {
			return nil, &elementError{
				element: "array index " + strconv.Itoa(i) +
					" >= " + strconv.Itoa(len(array.Value)),
				inner: ErrNoElement,
			}
		}
		return array.Value[i], nil
	default:
		return nil, &syntax.IncompatibleTypeError{
			Message: fmt.Sprintf("can't take array element from %T", array),
		}
	}
}

func (node *Node) matchFork(ref map[*syntax.CallStm]syntax.CollectionIndex,
	fork ForkId) (*Fork, error) {
	node.ensureForks()
	if len(node.forkRoots) == 0 {
		return node.forks[0], nil
	}
	matchedFork, err := fork.Match(ref, node.forkRoots)
	if err != nil {
		return nil, err
	}
	if matchedFork.determined() {
		return newFork(node, matchedFork), nil
	}
	node = node.view(matchedFork)
	for i, id := range node.forkIds.List {
		if id.Matches(matchedFork) {
			return node.forks[i], nil
		}
	}
	return nil, fmt.Errorf(
		"unresolved fork %s (from %s): no matches out of %d possible forks",
		matchedFork.GoString(), fork.GoString(), len(node.forkRoots))
}

// Find all of the forks for which the given fork could match a more-constrained
// version of the ID.
func (node *Node) matchForks(fork ForkId) []*Fork {
	node = node.view(fork)
	if len(node.forkRoots) == 0 {
		return node.forks
	}
	match := func(i int, f ForkId, forks []*Fork) bool {
		if len(forks) > i {
			return f.Matches(forks[i].forkId)
		}
		return false
	}
	forks := node.forks
	// Trim the matched parts off of the front.
	for len(forks) > 0 && !match(0, fork, forks) {
		forks = forks[1:]
	}
	// Trim the matched parts off the end.
	for len(forks) > 1 && !match(len(forks)-1, fork, forks) {
		forks = forks[:len(forks)-1]
	}
	if len(forks) <= 2 {
		return forks
	}
	result := forks[:1]
	if cap(result) != cap(forks) {
		panic(cap(result))
	}
	for i := 1; i < len(forks)-1; i++ {
		if match(i, fork, forks) {
			// if we haven't reallocated, this is a no-op.
			result = append(result, forks[i])
		} else if cap(result) == cap(forks) {
			// Need to allocate a new buffer.  It's capacity will be sufficient
			// to hold all but one element of upstream.  We know we're
			// skipping at least one, so it won't ever need to grow, and being
			// smaller than the length of upstream means the capacity will no
			// longer match either.
			result = make([]*Fork, i-1, len(forks)-1)
			copy(result, forks[:i])
		}
	}
	return append(result, forks[len(forks)-1])
}

func (f *Fork) resolveRef(binding *syntax.RefExp, t syntax.Type,
	decId string,
	readSize int64) (bool, json.Marshaler, error) {
	args, err := f.outs()
	if err != nil {
		if errors.Is(err, ErrNotStaged) {
			f.node.top.unstaged = append(f.node.top.unstaged, err)
			return false, nil, nil
		}
		return true, nil, &elementError{
			element: "stage " + binding.Id,
			inner:   err,
		}
	}
	types := f.node.top.types
	value, err := args.Path(binding.OutputId,
		types.Get(syntax.TypeId{
			Tname: decId,
		}), t, types)
	value = fromCore(value)
	if err != nil {
		err = &elementError{
			element: "stage " + binding.Id,
			inner:   err,
		}
	}
	return true, value, err
}

func (self *Fork) expandForkPart(must bool,
	i int, part *ForkSourcePart,
	split *syntax.SplitExp, result []ForkId) ([]ForkId, error) {
	if self.disabledState {
		return nil, nil
	}
	exp, err := split.BindingPath("", self.forkId.SourceIndexMap(),
		self.node.top.types)
	if err != nil {
		return nil, err
	}
	if sp, ok := exp.(*syntax.SplitExp); ok {
		return self.expandForkPartFromSource(must, i, part, split, sp.Source, result[len(result):])
	} else {
		return nil, nil
	}
}

func (forkId ForkId) getUnmatchedForkParts(bNode *Node) []*ForkSourcePart {
	if rl := forkId.UnmatchedParts(bNode.call.ForkRoots()); len(rl) > 0 {
		bNode = bNode.top.allNodes[rl[0].GetFqid()].view(forkId)
		parts := make([]*ForkSourcePart, 0, len(bNode.forks))
		for _, f := range bNode.forks {
			if forkId.Matches(f.forkId) {
				for _, part := range f.forkId {
					if part.Split.Call == rl[0].Call() {
						parts = append(parts, part)
					}
				}
			}
		}
		return parts
	}
	return nil
}

func (self *Fork) expandForkFromRef(must bool, i int,
	part *ForkSourcePart,
	split *syntax.SplitExp,
	ref *syntax.RefExp,
	result []ForkId) ([]ForkId, error) {
	bNode := self.node.top.allNodes[ref.Id]
	if bNode == nil {
		panic("invalid reference to " + ref.Id)
	}
	if bNode.call != self.node.call {
		bNode = bNode.view(self.forkId)
	}
	if parts := self.getUnmatchedForkParts(bNode); len(parts) > 0 {
		flen := len(self.forkId)
		if flen == 0 {
			panic("No forks to expand parts into")
		}
		// Allocate the results in a single block, which we're going to
		// take slices out of, to make life easier for the GC.
		rep := make(ForkId, flen*(len(parts)-1))
		if cap(result) < len(parts)-1 {
			result = make([]ForkId, 0, len(parts)-1)
		}
		for j, part := range parts {
			if j == 0 {
				self.forkId[i] = part
				self.updateId(self.forkId)
			} else {
				result = append(result, rep[:flen:flen])
				rep = rep[flen:]
				fid := result[j-1]
				for k, p := range self.forkId {
					if k == i {
						fid[k] = part
					} else {
						fid[k] = p
					}
				}
			}
		}
		return result, nil
	}
	matchedForks := bNode.matchForks(self.forkId)
	if len(matchedForks) != 1 {
		return nil, fmt.Errorf(
			"fork %s matched %d out of %d forks of %s, need exactly 1",
			self.forkId.GoString(),
			len(matchedForks), len(bNode.forks), bNode.GetFQName())
	}
	ready, obj, err := matchedForks[0].resolveRef(ref, nil,
		bNode.call.Call().DecId, readSizeLimit)
	if err != nil {
		return nil, &elementError{
			element: "evaluating mapping source " + ref.GoString(),
			inner:   err,
		}
	}
	if !ready {
		return nil, nil
	}
	return self.expandForkFromObj(i, part, split, obj, ref, result)
}

func (self *Node) GetFQName() string { return self.call.GetFqid() }

// expandForks is core Node.expandForks (node.go:481), its error kept on the
// node rather than written to the fork's metadata.
func (self *Node) expandForks(must bool) bool {
	self.ensureForks()

	any := false

	for i := 0; i < len(self.forks); i++ {
		fork := self.forks[i]

		newForks, err := fork.expand(must)
		for ; err == nil && len(newForks) > 0; newForks, err = fork.expand(must) {
			any = true

			if len(self.forkIds.List) == 0 {
				self.forkIds.List = make([]ForkId, 1, len(newForks)+1)
			}

			self.forkIds.List[i] = fork.forkId

			for _, id := range newForks {
				self.forks = append(self.forks, newFork(self, id))
				self.forkIds.List = append(self.forkIds.List, id)
			}
		}

		if err != nil {
			if must && self.expandErr == nil {
				self.expandErr = fmt.Errorf("computing forks of %s: %w", self.GetFQName(), err)
			}

			return any
		}

		if len(self.forkIds.List) > 0 {
			self.forkIds.List[i] = fork.forkId
		}
	}

	return any
}

func (self *Fork) updateId(id ForkId) {
	self.forkId = id
	self.fqname = self.node.call.GetFqid() + " " + id.key()
}

func (self *Fork) writeDisable() { self.disabledState = true }

func (self *Fork) disabled() (bool, error) {
	top := self.node.top
	var errs syntax.ErrorList
	for _, p := range self.forkId {
		if r := p.Range; r != nil && r.Length() == 0 {
			return true, nil
		}
	}
	for _, bind := range self.node.call.Disabled() {
		if ready, res, err := top.resolve(bind,
			top.types.Get(syntax.TypeId{
				Tname: syntax.KindBool,
			}), self.forkId, 0); err != nil {
			errs = append(errs, err)
		} else if ready {
			if res != nil {
				switch d := res.(type) {
				case *syntax.BoolExp:
					if d.Value {
						return true, nil
					}
				case *syntax.NullExp:
					errs = append(errs, fmt.Errorf(
						"disabled is bound to a null value, which the compiler should not allow"))
				case json.RawMessage:
					var v bool
					if err := json.Unmarshal(d, &v); err == nil && v {
						return true, nil
					} else if err != nil {
						if bytes.Equal(d, nullBytes) {
							errs = append(errs, fmt.Errorf(
								"disabled is bound to a null value, which is not permitted"))
						} else {
							errs = append(errs, err)
						}
					}
				}
			} else {
				errs = append(errs, fmt.Errorf(
					"disabled is bound to a null value, which the compiler should not allow"))
			}
		} else {
			errs = append(errs, top.notReady())
		}
	}
	return false, errs.If()
}
