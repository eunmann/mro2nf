package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"sync"
)

// Roots are the read-only trees every task of a run reads at one path: the
// S3 mount, the entry inputs a run reads in place where its tasks share
// the host's paths, and the pipeline's code (WithCode). A path under a root
// names the same file in every task, as a path mrp hands a stage does, so a
// bundle keeps it as the string it is, with no copy or link of its tree. A
// path in a dir Except names, the task's own work dir, is under no root:
// what a stage writes there is its own output. The zero Roots hold nothing.
type Roots struct {
	input, code rootSet
	except      []string
}

// rootSet is a set of roots by their given paths, with their real paths
// read when a path needs them.
type rootSet struct {
	given map[string]bool
	real  *realRoots
}

// realRoots are the real paths of the roots, which a stage output linking
// into one resolves to. A task names its inputs by the paths it was given,
// so they are read only for a path no given root or excepted dir settles,
// once for every copy of the Roots: a run reads thousands of entry files
// in place, each a lookup per path component to resolve, a remote call on
// a network filesystem, and a task reads few of them.
type realRoots struct {
	paths []string
	read  func(string) (string, error)
	once  sync.Once
	set   map[string]bool
	err   error
}

// NewRoots are the roots at paths, each by its cleaned path and, once it
// exists, its real path; a relative or empty path adds none.
func NewRoots(paths ...string) Roots {
	return Roots{input: newRootSet(paths)}
}

// WithCode is r also holding the trees at paths, the pipeline's code, each
// as NewRoots holds a root. A stage's output path under one stays the path
// it is, as under any root; but no input the task was given lies there, so
// the confinement treats a file the stage linked into it as any other path
// outside the task.
func (r Roots) WithCode(paths ...string) Roots {
	r.code = newRootSet(paths)

	return r
}

// newRootSet is the set of roots at paths, as NewRoots makes it.
func newRootSet(paths []string) rootSet {
	given := map[string]bool{}

	for _, p := range paths {
		if filepath.IsAbs(p) {
			given[filepath.Clean(p)] = true
		}
	}

	if len(given) == 0 {
		return rootSet{}
	}

	return rootSet{given: given, real: &realRoots{paths: slices.Sorted(maps.Keys(given)), read: filepath.EvalSymlinks}}
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
// under one, by its given or its real path, and lies in no dir r excepts.
func (r Roots) Holds(p string) (bool, error) {
	return r.holds(p, r.code, r.input)
}

// holdsInput is Holds for the roots of what the run gives its tasks, the
// S3 mount and the entry inputs read in place, and not its code.
func (r Roots) holdsInput(p string) (bool, error) {
	return r.holds(p, r.input)
}

// holds is Holds for the roots of sets, reading the real paths of each, in
// turn, only when no given path settles p.
func (r Roots) holds(p string, sets ...rootSet) (bool, error) {
	c, settled, held := r.settle(p, sets)
	if settled {
		return held, nil
	}

	for _, s := range sets {
		if s.real == nil {
			continue
		}

		set, err := s.real.resolve()
		if err != nil {
			return false, err
		}

		if underAny(c, set) {
			return true, nil
		}
	}

	return false, nil
}

// settle is the absolute path p cleaned, and whether the roots of sets
// settle if they hold p without their real paths, and if so, whether they
// do.
func (r Roots) settle(p string, sets []rootSet) (string, bool, bool) {
	if !filepath.IsAbs(p) || !slices.ContainsFunc(sets, func(s rootSet) bool { return len(s.given) > 0 }) {
		return "", true, false
	}

	c := filepath.Clean(p)
	if withinAny(c, r.except) {
		return c, true, false
	}

	for _, s := range sets {
		if underAny(c, s.given) {
			return c, true, true
		}
	}

	return c, false, false
}

// holdsGiven reports whether r holds the absolute path p by the given path
// of an input root.
func (r Roots) holdsGiven(p string) bool {
	_, settled, held := r.settle(p, []rootSet{r.input})

	return settled && held
}

// resolve is the set of the roots' real paths, read once.
func (rr *realRoots) resolve() (map[string]bool, error) {
	rr.once.Do(func() {
		rr.set = map[string]bool{}

		for _, p := range rr.paths {
			resolved, err := rr.read(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			if err != nil {
				rr.err = fmt.Errorf("roots: resolve %s: %w", p, err)

				return
			}

			rr.set[resolved] = true
		}
	})

	return rr.set, rr.err
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
