package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
)

// Roots are the read-only trees every task of a run reads at one path: the
// S3 mount, and the entry inputs a run reads in place where its tasks share
// the host's paths. A path under a root names the same file in every task,
// as a path mrp hands a stage does, so a bundle keeps it as the string it
// is, with no copy or link of its tree. A path in a dir Except names, the
// task's own work dir, is under no root: what a stage writes there is its
// own output. The zero Roots hold nothing.
type Roots struct {
	set    map[string]bool
	except []string
}

// NewRoots are the roots at paths, each cleaned; a relative or empty path,
// and one under another root, adds none.
func NewRoots(paths ...string) Roots {
	all := map[string]bool{}

	for _, p := range paths {
		if filepath.IsAbs(p) {
			all[filepath.Clean(p)] = true
		}
	}

	set := map[string]bool{}

	for p := range all {
		if parent := filepath.Dir(p); parent == p || !underAny(parent, all) {
			set[p] = true
		}
	}

	return Roots{set: set}
}

// Except is r holding no path in dir, spelled by its absolute path or, once
// dir exists, its real path.
func (r Roots) Except(dir string) (Roots, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Roots{}, fmt.Errorf("roots: resolve %s: %w", dir, err)
	}

	r.except = append(slices.Clone(r.except), abs)

	resolved, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}

	if err != nil {
		return Roots{}, fmt.Errorf("roots: resolve %s: %w", dir, err)
	}

	if resolved != abs {
		r.except = append(r.except, resolved)
	}

	return r, nil
}

// Holds reports whether the absolute path p, cleaned, is a root or lies
// under one, and lies in no dir r excepts.
func (r Roots) Holds(p string) bool {
	if len(r.set) == 0 || !filepath.IsAbs(p) {
		return false
	}

	c := filepath.Clean(p)

	return underAny(c, r.set) && !withinAny(c, r.except)
}

// Paths are the roots, sorted.
func (r Roots) Paths() []string {
	return slices.Sorted(maps.Keys(r.set))
}

// underAny reports whether the clean absolute path p or one of its
// ancestors is in set: a lookup per path component, however many roots.
func underAny(p string, set map[string]bool) bool {
	for {
		if set[p] {
			return true
		}

		parent := filepath.Dir(p)
		if parent == p {
			return false
		}

		p = parent
	}
}
