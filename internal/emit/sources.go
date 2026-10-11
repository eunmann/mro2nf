package emit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrExternalFile is returned for a stage src or entry file default the
// transpile would copy into the project or image although it resolves outside
// the .mro's directory and the -mropath roots.
var ErrExternalFile = errors.New("file outside the .mro directory and the -mropath roots")

// sourceRoots bounds the host files a transpile copies into the project or
// image, so a shared .mro cannot point a default or a src at, say, ~/.aws and
// have it shipped.
type sourceRoots struct {
	dirs  []string
	allow bool
}

// newSourceRoots resolves the .mro's directory and the -mropath roots. A root
// that does not resolve holds no file, so it is dropped.
func newSourceRoots(opts Options) sourceRoots {
	r := sourceRoots{allow: opts.AllowExternalFiles}

	for _, d := range append([]string{opts.MRODir}, opts.MROPath...) {
		if d == "" {
			continue
		}

		if resolved, err := realPath(d); err == nil {
			r.dirs = append(r.dirs, resolved)
		}
	}

	return r
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}

	return resolved, nil
}

func (r sourceRoots) contains(resolved string) bool {
	for _, d := range r.dirs {
		if rel, err := filepath.Rel(d, resolved); err == nil && filepath.IsLocal(rel) {
			return true
		}
	}

	return false
}

// check refuses path, named by what in the error, when it or any symlink the
// copy would follow below it resolves outside every root. A path that does
// not exist, or a dangling link, is not copied, so it passes.
func (r sourceRoots) check(what, path string) error {
	if r.allow {
		return nil
	}

	resolved, err := realPath(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}

	if !r.contains(resolved) {
		return refuseExternal(what, path, resolved)
	}

	return r.checkLinksBelow(what, resolved)
}

func refuseExternal(what, path, resolved string) error {
	loc := fmt.Sprintf("%q", path)
	if abs, err := filepath.Abs(path); err != nil || abs != resolved {
		loc += " (resolving to " + resolved + ")"
	}

	return fmt.Errorf("%s %s: %w; move it under the .mro's directory or a -mropath root, "+
		"or pass -allow-external-files to copy it anyway", what, loc, ErrExternalFile)
}

// checkLinksBelow walks the directory top and every directory a symlink in it
// leads to, refusing a link that resolves outside every root. Each directory
// is walked once, which bounds the walk and breaks link cycles.
func (r sourceRoots) checkLinksBelow(what, top string) error {
	pending := []string{top}
	walked := map[string]bool{}

	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		if walked[dir] {
			continue
		}

		walked[dir] = true

		linked, err := r.checkDirLinks(what, dir)
		if err != nil {
			return err
		}

		pending = append(pending, linked...)
	}

	return nil
}

// checkDirLinks checks every symlink below dir (walked without following
// links) and returns the directories the in-root links lead to. dir may be a
// plain file, which holds no links.
func (r sourceRoots) checkDirLinks(what, dir string) ([]string, error) {
	var linked []string

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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

		if !r.contains(target) {
			return refuseExternal(what, p, target)
		}

		if info, err := os.Stat(target); err == nil && info.IsDir() {
			linked = append(linked, target)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}

	return linked, nil
}
