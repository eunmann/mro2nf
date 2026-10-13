package shim

import (
	"path/filepath"
)

// Roots are the read-only trees every task of a run reads at one path, such
// as the S3 mount. A path under a root names the same file in every task, as
// a path mrp hands a stage does, so a bundle keeps it as the string it is,
// with no copy or link of its tree. The zero Roots hold nothing.
type Roots struct {
	set map[string]bool
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

// Holds reports whether the absolute path p, cleaned, is a root or lies
// under one.
func (r Roots) Holds(p string) bool {
	return len(r.set) > 0 && filepath.IsAbs(p) && underAny(filepath.Clean(p), r.set)
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
