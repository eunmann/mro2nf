package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/fileio"
)

// shapeFile names the file beside a leaf in its ordinal dir that records
// what of the leaf's tree its bundle cannot carry through an object store,
// which mrp hands a reader as the stage wrote it: each empty dir, which an
// upload of the bundle creates no object for, and each link the stage wrote
// that reaches nothing, which an upload refuses (aws s3 cp exits 2 on it).
// A reader recreates what it records (Staged.Restore).
const shapeFile = ".mre-shape"

var (
	// errShapeMismatch reports a staged leaf holding, at a path its shape
	// records, something other than what the shape records there.
	errShapeMismatch = errors.New("staged leaf differs from its recorded shape")
	// errBadShape reports a shape path outside its ordinal dir.
	errBadShape = errors.New("malformed leaf shape")
)

// leafShape is a leaf's shape record. Each path is slash-separated and
// relative to the leaf's ordinal dir, so it begins with the leaf's name.
type leafShape struct {
	Dirs  []string    `json:"dirs,omitempty"`
	Links []shapeLink `json:"links,omitempty"`
}

// shapeLink is a link that reaches nothing, by its path and the target
// text the stage gave it.
type shapeLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

// shapeRecorder collects the shape of the leaf a copy writes below the
// ordinal dir.
type shapeRecorder struct {
	ordinal string
	shape   leafShape
}

// link records the link src, which reaches nothing, as the copy's path dst.
func (r *shapeRecorder) link(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("read link %s: %w", src, err)
	}

	rel, err := r.rel(dst)
	if err != nil {
		return err
	}

	r.shape.Links = append(r.shape.Links, shapeLink{Path: rel, Target: target})

	return nil
}

// emptyDir records the copy's dir dst, which holds nothing.
func (r *shapeRecorder) emptyDir(dst string) error {
	rel, err := r.rel(dst)
	if err != nil {
		return err
	}

	r.shape.Dirs = append(r.shape.Dirs, rel)

	return nil
}

func (r *shapeRecorder) rel(dst string) (string, error) {
	rel, err := filepath.Rel(r.ordinal, dst)
	if err != nil {
		return "", fmt.Errorf("shape of %s: %w", dst, err)
	}

	return filepath.ToSlash(rel), nil
}

// write writes the shape into the ordinal dir, and nothing when it records
// nothing.
func (r *shapeRecorder) write() error {
	if len(r.shape.Dirs) == 0 && len(r.shape.Links) == 0 {
		return nil
	}

	raw, err := json.Marshal(r.shape)
	if err != nil {
		return fmt.Errorf("shape of %s: %w", r.ordinal, err)
	}

	return writeRaw(filepath.Join(r.ordinal, shapeFile), raw)
}

// Restore recreates, in each leaf of the items s holds, what the leaf's
// shape records and the staged leaf lacks, before the task reads it. A
// staged leaf is the producer's bundle where the work dir is shared, and
// the copy an object store kept of it where it is not.
func (s Staged) Restore() error {
	ordinals, err := s.ordinals()
	if err != nil {
		return err
	}

	for _, o := range ordinals {
		if !ordinalDir.MatchString(filepath.Base(o)) {
			continue
		}

		if err := restoreShape(o); err != nil {
			return err
		}
	}

	return nil
}

// restoreShape recreates what the shape recorded in the ordinal dir names.
func restoreShape(ordinal string) error {
	raw, err := os.ReadFile(filepath.Join(ordinal, shapeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read %s: %w", shapeFile, err)
	}

	var shape leafShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return fmt.Errorf("decode shape of %s: %w", ordinal, err)
	}

	for _, d := range shape.Dirs {
		at, err := shapePath(ordinal, d)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(at, fileio.DirPerm); err != nil {
			return fmt.Errorf("restore dir %s: %w", at, err)
		}
	}

	for _, l := range shape.Links {
		at, err := shapePath(ordinal, l.Path)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(at), fileio.DirPerm); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(at), err)
		}

		if err := restoreLink(at, l.Target); err != nil {
			return err
		}
	}

	return nil
}

// shapePath is the path in the ordinal dir that the shape's path rel names.
func shapePath(ordinal, rel string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("%w: %q in %s", errBadShape, rel, ordinal)
	}

	return filepath.Join(ordinal, filepath.FromSlash(rel)), nil
}

// restoreLink links at to target, unless at is that link already, as when
// another reader of the same staged leaf restored it first.
func restoreLink(at, target string) error {
	err := os.Symlink(target, at)
	if err == nil {
		return nil
	}

	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("restore link %s: %w", at, err)
	}

	got, linked, err := readLink(at)
	if err != nil {
		return err
	}

	if !linked || got != target {
		return fmt.Errorf("%w: %s, want a link to %q", errShapeMismatch, at, target)
	}

	return nil
}
