package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
)

// Roots are the read-only trees every task of a run reads at one path: the
// S3 mount, the entry inputs a run reads in place where its tasks share
// the host's paths, and the pipeline's code (WithCode). A path under a root
// names the same file in every task, as a path mrp hands a stage does, so a
// bundle keeps it as the string it is, with no copy or link of its tree. A
// path in a dir Except names, the task's own work dir, is under no root:
// what a stage writes there is its own output. The zero Roots hold nothing.
type Roots struct {
	set, code map[string]bool
	except    []string
}

// NewRoots are the roots at paths, each by its cleaned path and, once it
// exists, its real path, which a stage output linking into it resolves to;
// a relative or empty path, and one under another root, adds none.
func NewRoots(paths ...string) (Roots, error) {
	set, err := rootSet(paths)
	if err != nil {
		return Roots{}, err
	}

	return Roots{set: set}, nil
}

// WithCode is r also holding the trees at paths, the pipeline's code, each
// as NewRoots holds a root. A stage's output path under one stays the path
// it is, as under any root; but no input the task was given lies there, so
// the confinement treats a file the stage linked into it as any other path
// outside the task.
func (r Roots) WithCode(paths ...string) (Roots, error) {
	code, err := rootSet(paths)
	if err != nil {
		return Roots{}, err
	}

	r.code = code

	return r, nil
}

// rootSet is the set of roots at paths, as NewRoots makes it.
func rootSet(paths []string) (map[string]bool, error) {
	all := map[string]bool{}

	for _, p := range paths {
		if !filepath.IsAbs(p) {
			continue
		}

		all[filepath.Clean(p)] = true

		resolved, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("roots: resolve %s: %w", p, err)
		}

		all[resolved] = true
	}

	set := map[string]bool{}

	for p := range all {
		if parent := filepath.Dir(p); parent == p || !underAny(parent, all) {
			set[p] = true
		}
	}

	return set, nil
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
	return r.holds(p, r.set) || r.holds(p, r.code)
}

// holdsInput is Holds for the roots of what the run gives its tasks, the
// S3 mount and the entry inputs read in place, and not its code.
func (r Roots) holdsInput(p string) bool {
	return r.holds(p, r.set)
}

func (r Roots) holds(p string, set map[string]bool) bool {
	if len(set) == 0 || !filepath.IsAbs(p) {
		return false
	}

	c := filepath.Clean(p)

	return underAny(c, set) && !withinAny(c, r.except)
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
