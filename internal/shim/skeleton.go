package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// SkeletonFile names the file in a bundle dir that holds the bundle's
// skeleton: each of its paths, its records' contents, and none of its
// leaves' bytes. A task that reads only what a bundle names and how its
// leaves are laid out, OUTS, stages the skeleton in its place and expands
// it (ExpandSkeleton), so no published file's bytes reach it (#610).
const SkeletonFile = ".mre-skeleton.json"

// errBadSkeleton reports a skeleton entry that names no path inside the
// bundle.
var errBadSkeleton = errors.New("malformed bundle skeleton")

// skeletonEntry is one path of a bundle: a dir, a record with its content,
// or any other file, which expands empty.
type skeletonEntry struct {
	Path   string  `json:"path"`
	Dir    bool    `json:"dir,omitempty"`
	Record *string `json:"record,omitempty"`
}

// writeSkeleton writes the skeleton of the bundle dir into it. It walks
// the bundle as copyTree copies a tree, following links and leaving out a
// dangling one, so a leaf dir's skeleton holds what its copy would. The
// files at the bundle's top and each leaf's origin and shape records are
// records, so a link a leaf's shape records reaches its expansion.
func writeSkeleton(dir string) error {
	var entries []skeletonEntry

	if err := skeletonWalk(dir, "", true, &entries, map[dirID]struct{}{}); err != nil {
		return fmt.Errorf("skeleton of %s: %w", dir, err)
	}

	raw, err := json.Marshal(struct {
		Entries []skeletonEntry `json:"entries"`
	}{entries})
	if err != nil {
		return fmt.Errorf("skeleton of %s: %w", dir, err)
	}

	return writeRaw(filepath.Join(dir, SkeletonFile), raw)
}

func skeletonWalk(src, rel string, top bool, out *[]skeletonEntry, ancestors map[dirID]struct{}) error {
	resolved, err := resolveSymlink(src)
	if err != nil || resolved == "" {
		return err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolved, err)
	}

	if !info.IsDir() {
		return skeletonFile(resolved, rel, top, out)
	}

	release, cycle := enterDir(info, ancestors)
	if cycle {
		return fmt.Errorf("walk %s: %w", resolved, errSymlinkCycle)
	}

	defer release()

	if rel != "" {
		*out = append(*out, skeletonEntry{Path: rel, Dir: true})
	}

	names, err := os.ReadDir(resolved)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", resolved, err)
	}

	for _, e := range names {
		if rel == "" && e.Name() == SkeletonFile {
			continue
		}

		if err := skeletonWalk(filepath.Join(resolved, e.Name()), path.Join(rel, e.Name()), rel == "", out, ancestors); err != nil {
			return err
		}
	}

	return nil
}

// skeletonFile adds the file at src, at rel in the bundle, with its content
// when it is a record: a file at the bundle's top, or a leaf's origin or
// shape.
func skeletonFile(src, rel string, top bool, out *[]skeletonEntry) error {
	e := skeletonEntry{Path: rel}

	if top || ordinalRecord(rel) {
		raw, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}

		content := string(raw)
		e.Record = &content
	}

	*out = append(*out, e)

	return nil
}

// ordinalRecord reports whether the bundle path rel is a record in a leaf's
// ordinal dir, beside the leaf.
func ordinalRecord(rel string) bool {
	base := path.Base(rel)

	return path.Dir(path.Dir(rel)) == bundleFiles && (base == originFile || base == shapeFile)
}

// CopyShape writes at dst the shape of the tree at src: each dir, an empty
// file at each file, links followed as CopyTree copies the tree, and a link
// that reaches nothing as itself, as a reader of the leaf restores it. It
// reads no file's bytes.
func CopyShape(src, dst string) error {
	return copyShape(src, dst, map[dirID]struct{}{})
}

func copyShape(src, dst string, ancestors map[dirID]struct{}) error {
	resolved, err := resolveSymlink(src)
	if err != nil {
		return err
	}

	if resolved == "" {
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("read link %s: %w", src, err)
		}

		return linkAt(target, dst)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolved, err)
	}

	if !info.IsDir() {
		return writeRaw(dst, nil)
	}

	release, cycle := enterDir(info, ancestors)
	if cycle {
		return fmt.Errorf("shape of %s: %w", resolved, errSymlinkCycle)
	}

	defer release()

	if err := os.MkdirAll(dst, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", resolved, err)
	}

	for _, e := range entries {
		if err := copyShape(filepath.Join(resolved, e.Name()), filepath.Join(dst, e.Name()), ancestors); err != nil {
			return err
		}
	}

	return nil
}

// ExpandSkeleton writes, at dir, the bundle the skeleton file describes:
// its dirs, its records as they were written, and an empty file at each
// other path.
func ExpandSkeleton(file, dir string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read skeleton: %w", err)
	}

	var s struct {
		Entries []skeletonEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("decode skeleton %s: %w", file, err)
	}

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	for _, e := range s.Entries {
		if err := expandEntry(dir, e); err != nil {
			return fmt.Errorf("skeleton %s: %w", file, err)
		}
	}

	return nil
}

func expandEntry(dir string, e skeletonEntry) error {
	if !filepath.IsLocal(filepath.FromSlash(e.Path)) {
		return fmt.Errorf("%w: %q", errBadSkeleton, e.Path)
	}

	at := filepath.Join(dir, filepath.FromSlash(e.Path))

	if e.Dir {
		if err := os.MkdirAll(at, dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", at, err)
		}

		return nil
	}

	var content []byte
	if e.Record != nil {
		content = []byte(*e.Record)
	}

	return writeRaw(at, content)
}
