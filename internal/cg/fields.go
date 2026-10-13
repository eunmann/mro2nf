package cg

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// writeFields writes the posted instance's outs as one stamped bundle per
// declared output under dir/<output>/, each holding that output's value
// and only its files.
func (g *Graph) writeFields(in Instance, s *stagedStore, man types.Manifest, dir string) error {
	st, err := g.Stage(in.Node)
	if err != nil {
		return err
	}

	outs, err := s.outs(st, in.Fork)
	if err != nil {
		return err
	}

	params, err := man.Params(st.Call().DecId, types.RoleOut)
	if err != nil {
		return fmt.Errorf("out params of %s: %w", st.Fqid, err)
	}

	for _, p := range params {
		var v any

		if raw, ok := outs[p.Name]; ok {
			if v, err = types.DecodeJSON(raw); err != nil {
				return fmt.Errorf("decode %s output %s: %w", st.Fqid, p.Name, err)
			}
		}

		fd := filepath.Join(dir, p.Name)
		if err := shim.WriteBundle(fd, map[string]any{p.Name: v}, []ir.Param{p}, man.Table(), shim.Opts{}); err != nil {
			return fmt.Errorf("write %s output %s: %w", st.Fqid, p.Name, err)
		}

		if err := stampPosted(fd, in); err != nil {
			return err
		}
	}

	return nil
}

// writeGates writes to path the instances the posted instance disables, as
// GateTokens: each node with a disable binding that reads this stage alone,
// at every fork the binding resolves true (core/stage.go:883
// Fork.disabled). The binding resolves at this instance's fork extended by
// a fork of each root, beyond this stage's, it is forked on (gateRoots),
// which this stage feeds or the entry fixes, so this post enumerates them.
// A binding that does not resolve is left to the node's own task, which
// fails on it as mrp fails the fork.
func (g *Graph) writeGates(in Instance, s *stagedStore, path string) error {
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

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(strings.Join(off, " ")), filePerm); err != nil {
		return fmt.Errorf("write gates %s: %w", path, err)
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
		v, err := r.resolve(d, r.boolType(), a.fork)
		if err != nil {
			continue
		}

		if on, err := gateValue(v); err == nil && on {
			out = append(out, GateToken(st.Fqid, gate, strings.Join(a.ords, ".")))
		}
	}

	return out, nil
}
