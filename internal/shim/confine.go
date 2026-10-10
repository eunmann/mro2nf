package shim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errOutsideWork is returned when a stage output resolves to a path outside the
// task work dir that the stage did not receive as a staged input.
var errOutsideWork = errors.New("stage output resolves outside the task work dir")

// maxLinkHops bounds a symlink chain the confinement check follows, as the
// kernel's own ELOOP limit does.
const maxLinkHops = 40

// errLinkHops is returned for a symlink chain longer than maxLinkHops.
var errLinkHops = errors.New("too many levels of symbolic links")

// Confinement bounds which files a stage's outputs may pull into a bundle. The
// stage writes inside its phase dir (<work>/<phase>); the rest of the task work
// dir holds what Nextflow staged there as inputs and what mre wrote. An output
// leaf, and each symlink it passes through, must stay inside the work dir, or
// reach a path in it outside the phase dir: a staged input, which is followed
// wherever it points. Anything else (a link to a host file, an absolute host
// path) would copy a file the stage was never given into the bundle, where mrp
// would only publish a symlink, so the write fails instead.
type Confinement struct {
	work, phase []string
}

// ConfineStage returns the Confinement for the outputs of a stage phase
// ("split", "main" or "join") run in work.
func ConfineStage(work, phase string) (*Confinement, error) {
	workDirs, err := pathForms(work)
	if err != nil {
		return nil, err
	}

	phaseDirs, err := pathForms(filepath.Join(work, phase))
	if err != nil {
		return nil, err
	}

	return &Confinement{work: workDirs, phase: phaseDirs}, nil
}

// pathForms returns dir's absolute path and, when it differs, its real path, so
// a path spelled either way is recognized.
func pathForms(dir string) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}

	if resolved == abs {
		return []string{abs}, nil
	}

	return []string{abs, resolved}, nil
}

// within reports whether p is root or lies under it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)

	return err == nil && filepath.IsLocal(rel)
}

// withinAny reports whether p is within any of roots.
func withinAny(p string, roots []string) bool {
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}

	return false
}

// staged reports whether p lies in the work dir outside the phase dir and is
// not an ancestor of it.
func (c *Confinement) staged(p string) bool {
	for _, w := range c.work {
		if p == w || !within(p, w) {
			continue
		}

		if withinAny(p, c.phase) {
			return false
		}

		for _, ph := range c.phase {
			if within(ph, p) {
				return false
			}
		}

		return true
	}

	return false
}

// admit checks a leaf src against the confinement, following its symlinks one
// hop at a time. It reports true when src reaches a staged input, whose tree is
// then copied without further checks, and false when src is a file or dir of
// the stage's own inside the work dir, whose entries are checked in turn.
func (c *Confinement) admit(src string) (bool, error) {
	p, err := filepath.Abs(src)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", src, err)
	}

	for range maxLinkHops {
		if c.staged(p) {
			return true, nil
		}

		parent, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return false, fmt.Errorf("resolve %s: %w", p, err)
		}

		p = filepath.Join(parent, filepath.Base(p))
		if c.staged(p) {
			return true, nil
		}

		info, err := os.Lstat(p)
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", p, err)
		}

		if info.Mode()&os.ModeSymlink == 0 {
			if withinAny(p, c.work) {
				return false, nil
			}

			return false, fmt.Errorf("%w: %s resolves to %s", errOutsideWork, src, p)
		}

		target, err := os.Readlink(p)
		if err != nil {
			return false, fmt.Errorf("read link %s: %w", p, err)
		}

		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}

		p = filepath.Clean(target)
	}

	return false, fmt.Errorf("resolve %s: %w", src, errLinkHops)
}
