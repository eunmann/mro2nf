package cg

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
		if err := shim.WriteBundle(fd, map[string]any{p.Name: v}, []ir.Param{p}, man.Table(), nil); err != nil {
			return fmt.Errorf("write %s output %s: %w", st.Fqid, p.Name, err)
		}

		if err := writeInstance(fd, in); err != nil {
			return err
		}
	}

	return nil
}

// writeGates writes to path the nodes the posted instance disables at its
// fork: those with a disable binding that reads this stage alone and
// resolves true at the instance (core/stage.go:883 Fork.disabled). A node's
// instances read the binding at the fork they share with this instance, so
// one value serves them all. A binding that does not resolve is left to the
// node's own task, which fails on it as mrp fails the fork.
func (g *Graph) writeGates(in Instance, s *stagedStore, path string) error {
	r := &resolver{g: g, st: s}

	var off []string

	for _, fqid := range slices.Sorted(maps.Keys(g.stages)) {
		if gatedBy(g.stages[fqid], in.Node, func(d syntax.Exp) bool {
			v, err := r.resolve(d, r.boolType(), in.Fork)
			if err != nil {
				return false
			}

			on, err := gateValue(v)

			return err == nil && on
		}) {
			off = append(off, fqid)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(strings.Join(off, " ")), filePerm); err != nil {
		return fmt.Errorf("write gates %s: %w", path, err)
	}

	return nil
}

// gatedBy reports whether one of st's disable bindings reads producer alone
// and holds.
func gatedBy(st *syntax.CallGraphStage, producer string, holds func(syntax.Exp) bool) bool {
	for _, d := range st.Disabled() {
		if refs := refIDs(d); len(refs) == 1 && refs[0] == producer && holds(d) {
			return true
		}
	}

	return false
}
