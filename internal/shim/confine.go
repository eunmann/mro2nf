package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// errOutsideWork is returned when a strict confinement meets a stage output
// that resolves outside the task work dir to a path the stage did not receive
// as a staged input.
var errOutsideWork = errors.New("stage output resolves outside the task work dir")

// maxLinkHops bounds a symlink chain the confinement check follows, as the
// kernel's own ELOOP limit does.
const maxLinkHops = 40

// errLinkHops is returned for a symlink chain longer than maxLinkHops.
var errLinkHops = errors.New("too many levels of symbolic links")

// Confinement governs which files a stage's outputs may pull into a bundle, and
// how. The stage writes inside its phase dir (<work>/<phase>); the rest of the
// task work dir holds what Nextflow staged there as inputs and what mre wrote.
// An output leaf, and each symlink it passes through, either stays inside the
// work dir (the stage's own file, hard-linked into the bundle), reaches a path
// in it outside the phase dir (a staged input, followed wherever it points),
// lands outside the task inside what a staged input links to (the input's real
// path, which a stage that resolves its args' paths returns), or lands outside
// the task elsewhere (a host file the stage was never given).
//
// What happens to a host file depends on the target. On an isolated container
// target (strict) its path cannot be valid downstream and copying it would
// upload host data, so the write fails. On a shared filesystem mrp publishes a
// symlink to it and pipelines legitimately return paths into host reference
// dirs, so it is copied, never hard-linked, so no later write through the
// bundle can alias the host file.
type Confinement struct {
	work, phase []string
	strict      bool
	// roots hold what every task reads in place, an input the stage
	// received wherever the stage's own symlink to it lies.
	roots Roots
	// inputs are the real paths outside the work dir that its links outside
	// the phase dir resolve to, read the first time a leaf lands outside it.
	inputs     []string
	inputsRead bool
}

// leafKind classifies where a stage output leaf resolves.
type leafKind int

const (
	// leafOwn is the stage's own file or dir inside the work dir; a dir's
	// entries are classified in turn.
	leafOwn leafKind = iota
	// leafStaged reaches a Nextflow-staged input; its tree is copied as is.
	leafStaged
	// leafInput lands outside the task work dir, inside what a staged input
	// links to; its tree is copied as a staged input's is.
	leafInput
	// leafOutside lands outside the task work dir; a non-strict confinement
	// copies its tree byte for byte.
	leafOutside
)

// ConfineStage returns the Confinement for the outputs of a stage phase
// ("split", "main" or "join") run in work. strict refuses an output that
// resolves outside the task instead of copying it, unless it resolves under
// roots.
func ConfineStage(work, phase string, strict bool, roots Roots) (*Confinement, error) {
	workDirs, err := pathForms(work)
	if err != nil {
		return nil, err
	}

	phaseDirs, err := pathForms(filepath.Join(work, phase))
	if err != nil {
		return nil, err
	}

	return &Confinement{work: workDirs, phase: phaseDirs, strict: strict, roots: roots}, nil
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

// admit classifies a leaf src, following its symlinks one hop at a time. A
// strict confinement returns errOutsideWork, naming src and its target, where
// it would otherwise report leafOutside.
func (c *Confinement) admit(src string) (leafKind, error) {
	p, err := filepath.Abs(src)
	if err != nil {
		return leafOwn, fmt.Errorf("resolve %s: %w", src, err)
	}

	for range maxLinkHops {
		if c.staged(p) {
			return leafStaged, nil
		}

		parent, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return leafOwn, fmt.Errorf("resolve %s: %w", p, err)
		}

		p = filepath.Join(parent, filepath.Base(p))
		if c.staged(p) {
			return leafStaged, nil
		}

		info, err := os.Lstat(p)
		if err != nil {
			return leafOwn, fmt.Errorf("stat %s: %w", p, err)
		}

		if info.Mode()&os.ModeSymlink == 0 {
			return c.classify(src, p)
		}

		target, err := os.Readlink(p)
		if err != nil {
			return leafOwn, fmt.Errorf("read link %s: %w", p, err)
		}

		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}

		p = filepath.Clean(target)
	}

	return leafOwn, fmt.Errorf("resolve %s: %w", src, errLinkHops)
}

// classify places the real, non-symlink path p that leaf src resolved to.
func (c *Confinement) classify(src, p string) (leafKind, error) {
	if withinAny(p, c.work) {
		return leafOwn, nil
	}

	if c.roots.holdsInput(p) {
		return leafInput, nil
	}

	inputs, err := c.stagedInputs()
	if err != nil {
		return leafOwn, err
	}

	if withinAny(p, inputs) {
		return leafInput, nil
	}

	if c.strict {
		return leafOwn, fmt.Errorf("%w: %s resolves to %s", errOutsideWork, src, p)
	}

	return leafOutside, nil
}

// stagedInputs returns the real paths outside the work dir that the links in
// it outside the phase dir resolve to: what Nextflow staged, the args'
// links to it, and nothing the stage wrote. A dangling link stages nothing.
func (c *Confinement) stagedInputs() ([]string, error) {
	if c.inputsRead {
		return c.inputs, nil
	}

	err := filepath.WalkDir(c.work[0], func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if slices.Contains(c.phase, p) {
			return filepath.SkipDir
		}

		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}

		target, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("resolve %s: %w", p, err)
		}

		if !withinAny(target, c.work) {
			c.inputs = append(c.inputs, target)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read staged inputs of %s: %w", c.work[0], err)
	}

	c.inputsRead = true

	return c.inputs, nil
}
