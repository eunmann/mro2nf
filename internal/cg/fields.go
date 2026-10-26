package cg

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/fileio"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// writeFields writes the posted instance's outs a reader reads
// (postedOutputs) output by output: one that holds no file and is small
// (valueRecord) as a record in the spec's Values file, which the driver
// carries as a value, and any other as a stamped bundle under the spec's
// Fields dir, <output>/, holding that output's value and only its files,
// but those it names by reference, whose records the spec's FieldRefs file
// receives.
func (g *Graph) writeFields(in Instance, s *stagedStore, spec PostSpec) error {
	st, err := g.Stage(in.Node)
	if err != nil {
		return err
	}

	outs, err := s.outs(st, in.Fork)
	if err != nil {
		return err
	}

	params, err := g.postedOutputs(st)
	if err != nil {
		return err
	}

	var values, refs []string

	for _, p := range params {
		var v any

		if raw, ok := outs[p.Id]; ok {
			if v, err = types.DecodeJSON(raw); err != nil {
				return fmt.Errorf("decode %s output %s: %w", st.Fqid, p.Id, err)
			}
		}

		rec, isValue, err := g.valueRecord(in, p, v)
		if err != nil {
			return fmt.Errorf("%s output %s: %w", st.Fqid, p.Id, err)
		}

		if isValue {
			values = append(values, rec)

			continue
		}

		fd := filepath.Join(spec.Fields, p.Id)

		if err := stampPosted(fd, in); err != nil {
			return err
		}

		named, err := shim.WriteItem(fd, map[string]any{p.Id: v}, []*syntax.StructMember{p}, g.Table(), spec.Opts, spec.Staged)
		if err != nil {
			return fmt.Errorf("write %s output %s: %w", st.Fqid, p.Id, err)
		}

		refs = append(refs, shim.RefRecords(fd, named)...)
	}

	if err := fileio.Write(spec.FieldRefs, []byte(shim.JoinRefs(refs))); err != nil {
		return fmt.Errorf("post fields: %w", err)
	}

	if err := fileio.Write(spec.Values, []byte(strings.Join(values, " "))); err != nil {
		return fmt.Errorf("post fields: %w", err)
	}

	return nil
}

// postedOutputs are the out params of st's stage, in its order, that a
// reader reads (readsOf), every one where a reader reads the whole call. A
// post writes these alone: no task stages any other.
func (g *Graph) postedOutputs(st *syntax.CallGraphStage) ([]*syntax.StructMember, error) {
	params, err := g.Params(st.Call().DecId, types.RoleOut)
	if err != nil {
		return nil, fmt.Errorf("out params of %s: %w", st.Fqid, err)
	}

	read := map[string]bool{}

	for _, r := range g.readsOf(st.Fqid) {
		if r.Fields == nil {
			return params, nil
		}

		for _, f := range r.Fields {
			read[f] = true
		}
	}

	return slices.DeleteFunc(params, func(p *syntax.StructMember) bool { return !read[p.Id] }), nil
}

// writeGates writes to path the instances the posted instance disables, as
// GateTokens: each node with a disable binding that reads this stage alone,
// at every fork the binding resolves true (core/stage.go:883
// Fork.disabled). The binding resolves at this instance's fork extended by
// a fork of each root, beyond this stage's, it is forked on (gateRoots),
// which this stage feeds or the entry fixes, so this post enumerates them.
// A binding that does not resolve is left to the node's own task, which
// fails on it as mrp fails the fork.
func (g *Graph) writeGates(in Instance, s store, path string) error {
	r := &resolver{g: g, st: s}

	up, err := g.Stage(in.Node)
	if err != nil {
		return err
	}

	var off []string

	for st, d := range g.gatedBy(in.Node) {
		tokens, err := g.gateTokens(r, st, d, up, in.Fork)
		if err != nil {
			return err
		}

		off = append(off, tokens...)
	}

	if err := fileio.Write(path, []byte(strings.Join(off, " "))); err != nil {
		return fmt.Errorf("post gates: %w", err)
	}

	return nil
}

// gateTokens are the GateTokens of the forks at which d, a disable binding
// of st reading up, holds under the posting instance's fork.
func (g *Graph) gateTokens(r *resolver, st *syntax.CallGraphStage, d syntax.Exp, up *syntax.CallGraphStage, fork Fork) ([]string, error) {
	extra, err := g.gateRoots(st, d, up)
	if err != nil {
		return nil, err
	}

	type at struct {
		fork Fork
		ords []string
	}

	cur := []at{{fork: fork}}

	for _, root := range extra {
		var next []at

		for _, a := range cur {
			outer, _ := a.fork.restrict(g.outerRoots(root))

			idx, err := r.sourceIndices(root, outer)
			if err != nil {
				return nil, fmt.Errorf("gate forks of %s on %s: %w", st.Fqid, root.Fqid, err)
			}

			for i, ix := range idx {
				next = append(next, at{fork: a.fork.with(Part{Root: root, Index: ix}), ords: append(slices.Clip(a.ords), strconv.Itoa(i))})
			}
		}

		cur = next
	}

	gate := Gate{Node: up.Fqid, Roots: fqids(extra)}

	var out []string

	for _, a := range cur {
		if on, err := r.disables(d, a.fork); err == nil && on {
			out = append(out, GateToken(st.Fqid, gate, strings.Join(a.ords, ".")))
		}
	}

	return out, nil
}
