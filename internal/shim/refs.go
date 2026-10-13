package shim

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// refPrefix prefixes a file leaf a bundle names by reference (WriteItem):
// the remainder is the leaf's origin record, which is its identity.
const refPrefix = reservedPrefix + "ref:"

var (
	// errUnstagedRef reports a leaf a bundle names by reference that the
	// reading task did not stage.
	errUnstagedRef = errors.New("leaf named by reference is not staged")
	// errBadRef reports a staged ordinal dir that holds no one leaf beside
	// its origin record.
	errBadRef = errors.New("staged ordinal dir is not one recorded leaf")
)

// Staged are the dirs a task staged, each one item, its producer's bundle,
// or a dir of items, by the path the task was given it at: the item a file
// it names by reference lies in is named by that path.
type Staged struct {
	dirs []stagedDir
}

type stagedDir struct {
	abs, name string
}

// NewStaged are the staged dirs at the paths names, empty ones skipped.
func NewStaged(names ...string) (Staged, error) {
	var s Staged

	for _, n := range names {
		if n == "" {
			continue
		}

		abs, err := filepath.Abs(n)
		if err != nil {
			return Staged{}, fmt.Errorf("staged %s: %w", n, err)
		}

		s.dirs = append(s.dirs, stagedDir{abs: abs, name: n})
	}

	return s, nil
}

// Leaves indexes the leaf of each ordinal dir of the items s holds, the
// first of each origin, items in the order s was given them. A leaf the
// task staged with an item it reads is the one a reference to its origin
// resolves to, so the driver stages it by reference, as its ordinal dir
// alone (refs/<n>/f/<ordinal>/), only where no item the task stages holds
// it.
func (s Staged) Leaves() (Refs, error) {
	r := Refs{leaves: map[string]string{}}

	for _, d := range s.dirs {
		for _, pattern := range []string{filepath.Join(d.abs, bundleFiles, "*"), filepath.Join(d.abs, "*", bundleFiles, "*")} {
			ordinals, err := filepath.Glob(pattern)
			if err != nil {
				return Refs{}, fmt.Errorf("staged leaves %s: %w", d.name, err)
			}

			for _, ordinal := range ordinals {
				if err := r.index(ordinal); err != nil {
					return Refs{}, err
				}
			}
		}
	}

	return r, nil
}

// Ref names a whole bundle leaf a task staged: the item it lies in, by the
// path the task was given the item at (Staged), and its ordinal dir there,
// f/L<nnnn>, which holds the leaf and its origin record.
type Ref struct {
	Item, Ordinal string
}

// RefLines are the lines a task writes of the references in the bundle it
// wrote at dir, one per leaf: dir named by its last two path components,
// the item the leaf lies in and its ordinal dir there, tab-separated. The
// driver resolves each to where its producer wrote it from the paths it
// staged the task's items from.
func RefLines(dir string, refs []Ref) []string {
	item := filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
	out := make([]string, len(refs))

	for i, r := range refs {
		out[i] = item + "\t" + r.Item + "\t" + r.Ordinal + "\n"
	}

	return out
}

// item is the path the task was given the bundle dir at, the absolute path
// bundle, and false for a bundle it did not stage.
func (s Staged) item(bundle string) (string, bool) {
	for _, d := range s.dirs {
		switch {
		case bundle == d.abs:
			return d.name, true
		case filepath.Dir(bundle) == d.abs:
			return path.Join(d.name, filepath.Base(bundle)), true
		}
	}

	return "", false
}

// ref is the reference to the leaf at the absolute path abs, and the origin
// its ordinal dir records, when it is a whole leaf of a bundle the task
// staged: directly, or through the link LinkLeaves made of its ordinal dir.
// A path inside a directory leaf is no whole leaf.
func (s Staged) ref(abs string) (Ref, Origin, bool, error) {
	for range 2 {
		bundle, rel, ok := LeafBundle(abs)
		if !ok {
			return Ref{}, Origin{}, false, nil
		}

		ordinal := filepath.Dir(rel)

		if item, ok := s.item(bundle); ok {
			o, recorded, err := readOrdinalOrigin(filepath.Join(bundle, ordinal))

			return Ref{Item: item, Ordinal: filepath.ToSlash(ordinal)}, o, recorded, err
		}

		target, linked, err := readLink(filepath.Join(bundle, ordinal))
		if err != nil || !linked {
			return Ref{}, Origin{}, false, err
		}

		if !filepath.IsAbs(target) {
			target = filepath.Join(bundle, filepath.Dir(ordinal), target)
		}

		abs = filepath.Join(target, filepath.Base(abs))
	}

	return Ref{}, Origin{}, false, nil
}

// readLink is the target of the link at p, and false for a p that is no
// link.
func readLink(p string) (string, bool, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", false, fmt.Errorf("stat %s: %w", p, err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return "", false, nil
	}

	target, err := os.Readlink(p)
	if err != nil {
		return "", false, fmt.Errorf("read link %s: %w", p, err)
	}

	return target, true, nil
}

// WriteItem writes payload into dir as a bundle another task stages: every
// file leaf located by params/tbl is copied into its ordinal dir below
// dir/f/ beside its origin record and replaced with a marker, then the
// rewritten payload is written to dir/data.json. A leaf that is a whole leaf
// of a bundle the task staged (staged) it names by reference rather than
// copying it: a marker of the leaf's origin, which a reader resolves among
// the leaves it staged (Staged.Leaves), where its readers stage it from the
// item it lies in. It returns those references, one per origin.
func WriteItem(dir string, payload map[string]any, params []*syntax.StructMember, tbl *types.Table, opts Opts, staged Staged) ([]Ref, error) {
	refs := []Ref{}

	m := leafMarker{dir: dir, conf: opts.Confine, roots: opts.Roots, staged: staged, refs: &refs, marks: map[string]string{}}

	marked, err := markFiles(m, payload, params, tbl)
	if err != nil {
		return nil, err
	}

	return refs, writePayload(dir, marked)
}

// reference is the marker of the leaf at the absolute path abs, and true,
// when the writer names it by reference, recording the reference once per
// origin.
func (m leafMarker) reference(abs string) (string, bool, error) {
	if m.refs == nil {
		return "", false, nil
	}

	ref, o, ok, err := m.staged.ref(abs)
	if err != nil || !ok {
		return "", false, err
	}

	if marker, ok := m.marks[o.Path]; ok {
		return marker, true, nil
	}

	m.marks[o.Path] = refPrefix + o.record()
	*m.refs = append(*m.refs, ref)

	return m.marks[o.Path], true, nil
}

// copied is how many leaves the writer has copied or linked into the
// bundle.
func (m leafMarker) copied() int {
	if m.refs == nil {
		return len(m.marks)
	}

	return len(m.marks) - len(*m.refs)
}

// cutRef is the origin a reference marker names, and false for any other
// string.
func cutRef(s string) (Origin, bool) {
	record, ok := strings.CutPrefix(s, refPrefix)
	if !ok {
		return Origin{}, false
	}

	return parseOrigin(record)
}

// Refs are the whole bundle leaves a task staged, by origin, among which a
// reference resolves.
type Refs struct {
	leaves map[string]string
}

// index records the leaf of the ordinal dir at ordinal under its origin,
// unless a leaf of that origin is recorded.
func (r Refs) index(ordinal string) error {
	if !ordinalDir.MatchString(filepath.Base(ordinal)) {
		return nil
	}

	leaf, o, err := stagedLeaf(ordinal)
	if err != nil {
		return err
	}

	if _, ok := r.leaves[o.record()]; !ok {
		r.leaves[o.record()] = leaf
	}

	return nil
}

// stagedLeaf is the absolute path of the one leaf in an ordinal dir a task
// staged, and the origin it records.
func stagedLeaf(ordinal string) (string, Origin, error) {
	entries, err := os.ReadDir(ordinal)
	if err != nil {
		return "", Origin{}, fmt.Errorf("staged leaf %s: %w", ordinal, err)
	}

	o, recorded, err := readOrdinalOrigin(ordinal)
	if err != nil {
		return "", Origin{}, fmt.Errorf("staged leaf %s: %w", ordinal, err)
	}

	var names []string

	for _, e := range entries {
		if e.Name() != originFile {
			names = append(names, e.Name())
		}
	}

	if !recorded || len(names) != 1 {
		return "", Origin{}, fmt.Errorf("%w: %s holds %v", errBadRef, ordinal, names)
	}

	leaf, err := filepath.Abs(filepath.Join(ordinal, names[0]))
	if err != nil {
		return "", Origin{}, fmt.Errorf("staged leaf %s: %w", ordinal, err)
	}

	return leaf, o, nil
}

// leaf is the path of the staged leaf of origin o.
func (r Refs) leaf(o Origin) (string, error) {
	at, ok := r.leaves[o.record()]
	if !ok {
		return "", fmt.Errorf("%w: %s", errUnstagedRef, o.record())
	}

	return at, nil
}
