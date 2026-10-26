package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/fileio"
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
// the bundle as CopyTree copies a tree, following links and leaving out a
// dangling one, so a leaf dir's skeleton holds what its copy would. The
// files at the bundle's top and each leaf's origin and shape records are
// records, so a link a leaf's shape records reaches its expansion.
func writeSkeleton(dir string) error {
	var entries []skeletonEntry

	if err := walkTree(dir, skeletonNode{out: &entries}); err != nil {
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

// skeletonNode adds the path it visits, at rel in the bundle, to out; rel
// is "" at the bundle dir, and top is set at a path directly in it.
type skeletonNode struct {
	rel string
	top bool
	out *[]skeletonEntry
}

func (s skeletonNode) admit(string) (treeNode, error) { return s, nil }

func (s skeletonNode) dangling(string) error { return nil }

// file adds the file at src with its content when it is a record: a file
// at the bundle's top, or a leaf's origin or shape.
func (s skeletonNode) file(src string, _ fs.FileInfo) error {
	e := skeletonEntry{Path: s.rel}

	if s.top || ordinalRecord(s.rel) {
		raw, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}

		content := string(raw)
		e.Record = &content
	}

	*s.out = append(*s.out, e)

	return nil
}

func (s skeletonNode) dir() error {
	if s.rel != "" {
		*s.out = append(*s.out, skeletonEntry{Path: s.rel, Dir: true})
	}

	return nil
}

func (s skeletonNode) child(name string) (treeNode, bool) {
	if s.rel == "" && name == SkeletonFile {
		return nil, false
	}

	return skeletonNode{rel: path.Join(s.rel, name), top: s.rel == "", out: s.out}, true
}

func (s skeletonNode) leave(fs.FileInfo, bool) error { return nil }

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
	return walkTree(src, shapeNode{dst: dst})
}

// shapeNode places the shape of the path it visits at dst.
type shapeNode struct {
	dst string
}

func (s shapeNode) admit(string) (treeNode, error) { return s, nil }

func (s shapeNode) dangling(src string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("read link %s: %w", src, err)
	}

	return linkAt(target, s.dst)
}

func (s shapeNode) file(string, fs.FileInfo) error { return writeRaw(s.dst, nil) }

func (s shapeNode) dir() error {
	if err := os.MkdirAll(s.dst, fileio.DirPerm); err != nil {
		return fmt.Errorf("create %s: %w", s.dst, err)
	}

	return nil
}

func (s shapeNode) child(name string) (treeNode, bool) {
	return shapeNode{dst: filepath.Join(s.dst, name)}, true
}

func (s shapeNode) leave(fs.FileInfo, bool) error { return nil }

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

	if err := os.MkdirAll(dir, fileio.DirPerm); err != nil {
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
		if err := os.MkdirAll(at, fileio.DirPerm); err != nil {
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
