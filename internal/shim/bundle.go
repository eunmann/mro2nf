// Package shim reads and writes the bundle directories the generated tasks
// exchange (a JSON payload beside the files its leaves name, with the
// @mre: markers that locate them) and confines a stage's outputs to its work
// dir.
package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

const (
	filePerm = 0o644
	dirPerm  = 0o755
)

// errDestExists is returned by CopyTree when the destination already exists, to
// avoid truncating a possibly hard-linked source.
var errDestExists = errors.New("destination already exists")

// errSymlinkCycle is returned by CopyTree when a directory output reaches one of
// its own ancestors through a symlink, which would otherwise recurse unbounded.
var errSymlinkCycle = errors.New("directory symlink cycle")

// ErrBadMarker reports a payload string carrying the reserved marker prefix in
// a shape mre never writes: a corrupt or forged bundle.
var ErrBadMarker = errors.New("malformed bundle marker")

// The bundle is the object-store-portable channel item exchanged between
// Nextflow processes: a directory holding a JSON payload plus the actual files
// it references, so Nextflow stages files (not bare absolute paths) across task
// boundaries. File leaves in the payload are stored as markers; the real files
// live under the files subdir, named collision-free.
//
// Every payload string beginning with reservedPrefix is mre's own: a marker
// MarkFiles wrote at a file leaf the type walk found, or an escaped value.
// MarkFiles escapes any other string that begins with the prefix, whatever its
// type, so a user's string can never be read back as a marker; ReadBundle
// unescapes it, and the stage receives the string mrp would give it.
const (
	reservedPrefix = "@mre:"
	// FileMarker prefixes a plain-file leaf value in a bundle's payload. The
	// remainder is the bundle-relative path to the staged file.
	FileMarker = reservedPrefix + "file:"
	// DirMarker is FileMarker's counterpart for a leaf staged from a directory
	// (a Martian `path` output, or a directory a stage wrote into a `file`-typed
	// out). Both markers resolve to a path identically; the distinction records
	// the leaf's ground-truth dir-ness — measured by stat at staging time — so a
	// downstream consumer (publish's manifest) need not infer it from the declared
	// output type. See MarkFiles and cmd/mre/publish.go emitFile.
	DirMarker = reservedPrefix + "dir:"
	// escapePrefix prefixes a payload value that is not a marker but itself
	// begins with reservedPrefix; the remainder is the original string.
	escapePrefix = reservedPrefix + "str:"
	// bundleData is the payload filename inside a bundle directory.
	bundleData = "data.json"
	// bundleFiles is the files subdirectory inside a bundle directory.
	bundleFiles = "f"
)

// ordinalName is the name of a leaf's ordinal dir under the files subdir.
const ordinalName = `L[0-9]{4,}`

// markerRel is the only bundle-relative shape MarkFiles writes: the leaf's own
// basename in an ordinal dir directly under the files subdir.
var markerRel = regexp.MustCompile(`^` + bundleFiles + `/` + ordinalName + `/([^/\x00]+)$`)

var ordinalDir = regexp.MustCompile(`^` + ordinalName + `$`)

// originFile names the file beside a leaf in its ordinal dir that holds the
// leaf's origin. A file is one to mrp wherever it is referenced, so its origin
// travels with every copy of it, from bundle to bundle and through a stage
// that returns an input it was given: it is the absolute path the file was
// first staged from, where its task wrote it, the path mrp would name.
const originFile = ".mre-origin"

var (
	// errNoBasename reports a file leaf whose path names no file, such as "/".
	errNoBasename = errors.New("file leaf has no basename")
	// errReservedLeaf reports a file leaf named like the origin file it would
	// share its ordinal dir with.
	errReservedLeaf = errors.New("file leaf name is reserved")
	// errBadOrigin reports an origin file that holds no origin MarkFiles writes.
	errBadOrigin = errors.New("malformed leaf origin")
)

// validRel reports whether rel has markerRel's shape and stays in the bundle.
func validRel(rel string) bool {
	m := markerRel.FindStringSubmatch(rel)

	return m != nil && m[1] != "." && m[1] != ".." && filepath.IsLocal(rel)
}

// Marker returns the transport marker for a leaf staged at bundle-relative rel,
// choosing FileMarker or DirMarker by the leaf's stat'd dir-ness.
func Marker(rel string, isDir bool) string {
	if isDir {
		return DirMarker + rel
	}

	return FileMarker + rel
}

// CutMarker splits a transport marker into (rel, isDir, ok): its bundle-relative
// path and whether the leaf is a directory. ok is false for a string that is not
// a well-formed marker (a plain scalar, an escaped value, a raw path for a
// declared output never written, or a marker whose rel MarkFiles never writes).
func CutMarker(s string) (string, bool, bool) {
	rel, isDir := strings.CutPrefix(s, DirMarker)
	if !isDir {
		var ok bool
		if rel, ok = strings.CutPrefix(s, FileMarker); !ok {
			return "", false, false
		}
	}

	if !validRel(rel) {
		return "", false, false
	}

	return rel, isDir, true
}

// CheckReserved returns ErrBadMarker for a string that begins with the reserved
// prefix but is neither a well-formed marker nor an escaped value, and nil for
// any other string.
func CheckReserved(s string) error {
	if !strings.HasPrefix(s, reservedPrefix) || strings.HasPrefix(s, escapePrefix) {
		return nil
	}

	if _, _, ok := CutMarker(s); ok {
		return nil
	}

	return fmt.Errorf("%w: %q", ErrBadMarker, s)
}

// escapeValue returns a copy of v with every string that begins with the
// reserved prefix escaped, so no value MarkFiles did not itself mark reads back
// as a marker.
func escapeValue(v any) (any, error) {
	return mapStrings(v, func(s string) (string, error) {
		if strings.HasPrefix(s, reservedPrefix) {
			return escapePrefix + s, nil
		}

		return s, nil
	})
}

// mapStrings returns a copy of the decoded JSON tree v with fn applied to every
// string value (object keys, and their order, are left alone). A
// json.RawMessage is decoded first so its strings are reached too.
func mapStrings(v any, fn func(string) (string, error)) (any, error) {
	switch t := v.(type) {
	case string:
		return fn(t)
	case json.RawMessage:
		dv, err := decodeJSON(t)
		if err != nil {
			return nil, err
		}

		return mapStrings(dv, fn)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			ne, err := mapStrings(e, fn)
			if err != nil {
				return nil, err
			}

			out[i] = ne
		}

		return out, nil
	case types.Object:
		out := make(types.Object, len(t))
		for i, m := range t {
			ne, err := mapStrings(m.Value, fn)
			if err != nil {
				return nil, err
			}

			out[i] = types.Member{Key: m.Key, Value: ne}
		}

		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			ne, err := mapStrings(e, fn)
			if err != nil {
				return nil, err
			}

			out[k] = ne
		}

		return out, nil
	default:
		return v, nil
	}
}

// IsBundle reports whether dir holds a bundle payload.
func IsBundle(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, bundleData))

	return err == nil
}

// ReadBundle loads a bundle directory's payload and rewrites every file marker
// to an absolute path under the (staged) bundle, so callers see real paths, and
// restores every escaped value. An empty dir yields nil (an absent optional
// input). The rewrite needs no types: MarkFiles escapes every reserved string it
// did not mark, so a marker here is a file leaf the writer's type walk found.
func ReadBundle(dir string) (json.RawMessage, error) {
	if dir == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(filepath.Join(dir, bundleData))
	if err != nil {
		return nil, fmt.Errorf("read bundle %s: %w", dir, err)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle %s: %w", dir, err)
	}

	v, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse bundle %s: %w", dir, err)
	}

	resolved, err := mapStrings(v, func(s string) (string, error) {
		return resolveLeaf(s, abs)
	})
	if err != nil {
		return nil, fmt.Errorf("resolve bundle %s: %w", dir, err)
	}

	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("encode bundle %s: %w", dir, err)
	}

	return out, nil
}

// resolveLeaf maps one payload string read from the bundle at bundleAbs: an
// escaped value to its original, a marker to its absolute staged path, any
// other reserved string to ErrBadMarker, and everything else to itself.
func resolveLeaf(s, bundleAbs string) (string, error) {
	if orig, ok := strings.CutPrefix(s, escapePrefix); ok {
		return orig, nil
	}

	if rel, _, ok := CutMarker(s); ok {
		return filepath.Join(bundleAbs, rel), nil
	}

	if err := CheckReserved(s); err != nil {
		return "", err
	}

	return s, nil
}

// decodeJSON decodes JSON into an any tree, keeping numbers as json.Number so a
// whole-number float like 42.0 is not silently rewritten to 42 on re-encode, and
// objects as types.Object so a value re-encodes with its producer's key order.
func decodeJSON(raw json.RawMessage) (any, error) {
	v, err := types.DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode bundle json: %w", err)
	}

	return v, nil
}

// Opts are how a bundle writer treats the files a payload's leaves name.
// Confine confines a stage's outputs; it is nil for a payload a stage did not
// choose (bindings, merges, entry args), whose files the run was given. A
// leaf under Roots stays the path it is.
type Opts struct {
	Confine *Confinement
	Roots   Roots
}

// WriteBundle writes payload into dir as a bundle: every file leaf located by
// params/tbl is copied into dir/f/ and replaced with a marker, then the
// rewritten payload is written to dir/data.json.
func WriteBundle(dir string, payload map[string]any, params []*syntax.StructMember, tbl *types.Table, opts Opts) error {
	marked, err := MarkFiles(dir, payload, params, tbl, opts)
	if err != nil {
		return err
	}

	return writePayload(dir, marked)
}

// WriteLinkedBundle writes payload into dir as a bundle that no other task
// stages, such as a stage task's args: each file leaf is a symlink to
// the file payload names, which the task staged, not a copy of it. A leaf of
// a staged bundle is linked by its ordinal dir, so its recorded origin comes
// with it. A leaf under roots stays the path it is. A bundle another task
// stages must carry its files: WriteBundle.
func WriteLinkedBundle(dir string, payload map[string]any, params []*syntax.StructMember, tbl *types.Table, roots Roots) error {
	marked, err := markFiles(leafMarker{dir: dir, roots: roots, link: true, marks: map[string]string{}}, payload, params, tbl)
	if err != nil {
		return err
	}

	return writePayload(dir, marked)
}

// writePayload writes a decoded payload as dir/data.json.
func writePayload(dir string, payload map[string]any) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create bundle %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bundle %s: %w", dir, err)
	}

	return writeRaw(filepath.Join(dir, bundleData), data)
}

func writeRaw(path string, data []byte) error {
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	return nil
}

// MarkFiles copies every file leaf in payload (located via params/tbl) into
// dir/f/, replacing each with a bundle-relative marker, and returns the
// rewritten payload. Names are sequenced so distinct files never collide, and
// leaves of one origin share one copy. Every other string that begins with
// the reserved prefix is escaped, a file leaf naming no file included.
func MarkFiles(dir string, payload map[string]any, params []*syntax.StructMember, tbl *types.Table, opts Opts) (map[string]any, error) {
	return markFiles(leafMarker{dir: dir, conf: opts.Confine, roots: opts.Roots, marks: map[string]string{}}, payload, params, tbl)
}

func markFiles(m leafMarker, payload map[string]any, params []*syntax.StructMember, tbl *types.Table) (map[string]any, error) {
	escaped := make(map[string]any, len(payload))
	for k, v := range payload {
		ev, err := escapeValue(v)
		if err != nil {
			return nil, fmt.Errorf("escape bundle value %s: %w", k, err)
		}

		escaped[k] = ev
	}

	marked, err := tbl.Apply(params, escaped, m.mark)
	if err != nil {
		return nil, fmt.Errorf("collect bundle files: %w", err)
	}

	return marked, nil
}

// leafMarker copies, or with link set links, a payload's file leaves into the
// bundle dir. marks maps a copied leaf's origin to its marker: a file the
// payload reaches at several positions, or through several bundles, is one
// file to mrp, which publishes it once and names that one published path at
// each (post_process.go moveOutFile, copyOutSymlink), so it is copied once
// and every position gets the same marker.
type leafMarker struct {
	dir   string
	conf  *Confinement
	roots Roots
	link  bool
	marks map[string]string
}

// mark copies the file leaf to its own ordinal dir and returns its marker, or
// returns leaf unchanged when it names no file or lies under a root, where
// every task reads it, so neither its tree is walked nor its origin recorded.
func (m leafMarker) mark(leaf string) (string, error) {
	src := strings.TrimPrefix(leaf, escapePrefix)
	if src == "" || m.roots.Holds(src) {
		return leaf, nil
	}

	// A declared output file the stage legitimately never wrote: keep the path
	// string unchanged rather than erroring on stat. Martian keeps it as-is at
	// stage finalize and only nulls it at publish; a downstream join may still
	// need the path string. See bug 4. os.Stat (not Lstat) follows symlinks so
	// a dangling symlink is treated as absent too, rather than aborting the
	// bundle in CopyTree's symlink resolution (matching publish's emitFile).
	// The same stat records the leaf's ground-truth dir-ness for the marker;
	// a non-IsNotExist error falls through to CopyTree, which reports it
	// precisely.
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return leaf, nil
	}

	isDir := err == nil && info.IsDir()

	abs, err := filepath.Abs(src)
	if err != nil {
		return "", fmt.Errorf("resolve file leaf %s: %w", src, err)
	}

	origin, err := m.origin(src, abs)
	if err != nil {
		return "", err
	}

	if marker, ok := m.marks[origin.Path]; ok {
		return marker, nil
	}

	name, err := leafName(src, abs)
	if err != nil {
		return "", err
	}

	// Each leaf gets its own ordinal dir (f/L0000, f/L0001, … in canonical walk
	// order), staged downstream as one path item, so leaves sharing a basename
	// never collide; inside it the leaf keeps the basename mrp passes (#284).
	rel := filepath.Join(bundleFiles, fmt.Sprintf("L%04d", len(m.marks)), name)

	if err := m.stage(src, abs, rel, origin); err != nil {
		return "", err
	}

	m.marks[origin.Path] = Marker(rel, isDir)

	return m.marks[origin.Path], nil
}

// origin is the origin of the leaf at src, whose absolute path is abs, that
// the bundle leaf holding it recorded, or the one this write establishes for
// a leaf no bundle recorded.
func (m leafMarker) origin(src, abs string) (Origin, error) {
	o, ok, err := enclosingOrigin(abs)
	if err != nil || ok {
		return o, err
	}

	linked, err := m.linked(src)

	return Origin{Path: abs, Linked: linked}, err
}

// enclosingOrigin returns the origin of the path abs from the nearest bundle
// leaf that is or holds it: a file inside a staged directory leaf is, to mrp,
// that file inside the directory its producer wrote or the run was given, and
// mrp publishes it as it does that directory.
func enclosingOrigin(abs string) (Origin, bool, error) {
	for leaf := abs; leaf != filepath.Dir(leaf); leaf = filepath.Dir(leaf) {
		if _, _, ok := LeafBundle(leaf); !ok {
			continue
		}

		o, ok, err := ReadOrigin(leaf)
		if err != nil || !ok {
			return Origin{}, false, err
		}

		rel, err := filepath.Rel(leaf, abs)
		if err != nil {
			return Origin{}, false, fmt.Errorf("resolve %s in leaf %s: %w", abs, leaf, err)
		}

		return Origin{Path: filepath.Join(o.Path, rel), Linked: o.Linked}, true, nil
	}

	return Origin{}, false, nil
}

// linked reports whether mrp links the leaf src, which no bundle recorded:
// a file the run was given, written without a confinement, or a stage output
// that is a symlink or lies outside its task, such as the real path of an
// entry file its args name.
func (m leafMarker) linked(src string) (bool, error) {
	if m.conf == nil {
		return true, nil
	}

	info, err := os.Lstat(src)
	if err != nil {
		return false, fmt.Errorf("stat file leaf %s: %w", src, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}

	kind, err := m.conf.admit(src)

	return kind == leafInput || kind == leafOutside, err
}

// stage copies, or links, the leaf at src, whose absolute path is abs, to
// rel in the bundle and records its origin beside it.
func (m leafMarker) stage(src, abs, rel string, origin Origin) error {
	dst := filepath.Join(m.dir, rel)
	if m.link {
		return linkLeaf(abs, dst, origin)
	}

	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return fmt.Errorf("create files dir: %w", err)
	}

	if err := copyTree(src, dst, make(map[dirID]struct{}), copyPolicy{conf: m.conf}); err != nil {
		return err
	}

	return writeOrigin(filepath.Dir(dst), origin)
}

// linkLeaf links dst to the leaf at abs: its ordinal dir to the leaf's own,
// whose origin record it shares, when abs is a bundle leaf that recorded
// one, and otherwise the leaf itself, its origin recorded beside it.
func linkLeaf(abs, dst string, origin Origin) error {
	ordinal := filepath.Dir(dst)

	_, recorded, err := ReadOrigin(abs)
	if err != nil {
		return err
	}

	if recorded {
		if err := os.MkdirAll(filepath.Dir(ordinal), dirPerm); err != nil {
			return fmt.Errorf("create files dir: %w", err)
		}

		return linkAt(filepath.Dir(abs), ordinal)
	}

	if err := os.MkdirAll(ordinal, dirPerm); err != nil {
		return fmt.Errorf("create files dir: %w", err)
	}

	if err := linkAt(abs, dst); err != nil {
		return err
	}

	return writeOrigin(ordinal, origin)
}

func linkAt(target, link string) error {
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("link %s: %w", link, err)
	}

	return nil
}

// writeOrigin records origin in the ordinal dir of the leaf it is the
// origin of.
func writeOrigin(ordinal string, origin Origin) error {
	record := origin.Path
	if origin.Linked {
		record = linkedPrefix + record
	}

	return writeRaw(filepath.Join(ordinal, originFile), []byte(record))
}

// Origin is a file leaf's identity: the absolute path it was first staged
// from, where its task wrote it, and how mrp publishes it.
type Origin struct {
	Path string
	// Linked is a stage output that was a symlink, or a file from outside
	// every task: a host file, or one the run was given. mrp's
	// post-processing links such a file into outs/ at each output that
	// reaches it, where it moves a stage's own file to the first and links
	// the others to that (post_process.go moveOutFile, copyOutSymlink).
	Linked bool
}

// linkedPrefix begins the recorded origin of a Linked leaf; a recorded path
// is absolute, so the two never read alike.
const linkedPrefix = "linked:"

// ReadOrigin returns the origin recorded beside the file leaf at the absolute
// path abs, and false when abs is no leaf of a bundle or none is recorded.
func ReadOrigin(abs string) (Origin, bool, error) {
	if _, _, ok := LeafBundle(abs); !ok {
		return Origin{}, false, nil
	}

	recorded, err := os.ReadFile(filepath.Join(filepath.Dir(abs), originFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Origin{}, false, nil
	}

	if err != nil {
		return Origin{}, false, fmt.Errorf("read origin of %s: %w", abs, err)
	}

	path, linked := strings.CutPrefix(string(recorded), linkedPrefix)
	if !filepath.IsAbs(path) {
		return Origin{}, false, fmt.Errorf("%w: %q beside %s", errBadOrigin, recorded, abs)
	}

	return Origin{Path: path, Linked: linked}, true, nil
}

// LeafBundle returns the bundle dir holding the file leaf at path and the
// leaf's path in it, and false for a path that is no bundle leaf.
func LeafBundle(path string) (string, string, bool) {
	ordinal := filepath.Dir(path)
	files := filepath.Dir(ordinal)

	if !ordinalDir.MatchString(filepath.Base(ordinal)) || filepath.Base(files) != bundleFiles {
		return "", "", false
	}

	dir := filepath.Dir(files)

	return dir, filepath.Join(bundleFiles, filepath.Base(ordinal), filepath.Base(path)), true
}

// leafName returns the basename of the file leaf at src, whose absolute path
// is abs: the name mrp's stage sees, src being the path mrp would pass. A path
// ending in "/", "." or ".." names the directory it resolves to.
func leafName(src, abs string) (string, error) {
	name := filepath.Base(abs)
	if name == string(filepath.Separator) {
		return "", fmt.Errorf("%w: %q", errNoBasename, src)
	}

	if name == originFile {
		return "", fmt.Errorf("%w: %q", errReservedLeaf, src)
	}

	return name, nil
}

// CopyTree links or copies src to dst, recursing into directories (Martian file
// outputs may be directories). It hard-links files when possible (same device)
// for speed, falling back to a byte copy. It follows symlinks to any target,
// so it is for trees the run was given, not a stage's outputs.
func CopyTree(src, dst string) error {
	return copyTree(src, dst, make(map[dirID]struct{}), copyPolicy{})
}

// copyPolicy is how copyTree treats the tree it is copying: conf, while
// non-nil, classifies each entry of a stage's own output (see Confinement), and
// copyOnly forbids hard links below a host path outside the task.
type copyPolicy struct {
	conf     *Confinement
	copyOnly bool
}

// admit returns the policy for src and its entries: unchanged for the stage's
// own files, unconfined below a staged input or what one links to, and
// unconfined but copy-only below any other host path outside the task.
func (p copyPolicy) admit(src string) (copyPolicy, error) {
	if p.conf == nil {
		return p, nil
	}

	kind, err := p.conf.admit(src)
	if err != nil {
		return p, err
	}

	switch kind {
	case leafStaged, leafInput:
		return copyPolicy{}, nil
	case leafOutside:
		return copyPolicy{copyOnly: true}, nil
	default:
		return p, nil
	}
}

// resolveSymlink returns src, or its real target when src is a symlink, and ""
// for a dangling symlink. Nextflow stages inputs as symlinks, and link(2) does
// not follow them — hard-linking the symlink would leave a dangling link once
// the bundle is staged into another isolated task (AWS Batch + S3 /
// HealthOmics), where the original target is absent. A dangling symlink inside
// a copied directory is treated as absent — the same way MarkFiles' top-level
// guard does (os.Stat -> IsNotExist -> keep the path) — rather than aborting
// the whole bundle.
func resolveSymlink(src string) (string, error) {
	lst, err := os.Lstat(src)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", src, err)
	}

	if lst.Mode()&os.ModeSymlink == 0 {
		return src, nil
	}

	resolved, err := filepath.EvalSymlinks(src)
	if os.IsNotExist(err) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", src, err)
	}

	return resolved, nil
}

// dirID identifies a directory by (device, inode) — the same identity os.SameFile
// compares — so the copy-tree recursion detects a directory reached twice on one
// path regardless of the symlink route taken to it. int64 (not uint64) keeps the
// field conversions non-redundant across platforms whose Stat_t.Dev differs
// (linux uint64, darwin int32); sign is irrelevant to a map key.
type dirID struct {
	dev, ino int64
}

// enterDir records dir's (device, inode) in ancestors for cycle detection. The
// bool is true when it was already on the recursion stack (a cycle); the returned
// func pops it on unwind.
func enterDir(dir os.FileInfo, ancestors map[dirID]struct{}) (func(), bool) {
	st, ok := dir.Sys().(*syscall.Stat_t)
	if !ok {
		return func() {}, false
	}

	id := dirID{dev: int64(st.Dev), ino: int64(st.Ino)}
	if _, seen := ancestors[id]; seen {
		return func() {}, true
	}

	ancestors[id] = struct{}{}

	return func() { delete(ancestors, id) }, false
}

// copyTree implements CopyTree, threading the set of directory identities already
// on the recursion stack. A directory output that (via a symlink) points at one
// of its own ancestors would otherwise recurse until ENAMETOOLONG, duplicating
// the tree on every pass; catching the repeat (dev, inode) bounds the recursion
// to the finite number of distinct directories reachable once.
func copyTree(src, dst string, ancestors map[dirID]struct{}, pol copyPolicy) error {
	resolved, err := resolveSymlink(src)
	if err != nil || resolved == "" {
		return err
	}

	if pol, err = pol.admit(src); err != nil {
		return err
	}

	src = resolved

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	if info.IsDir() {
		release, cycle := enterDir(info, ancestors)
		if cycle {
			return fmt.Errorf("copy %s: %w", src, errSymlinkCycle)
		}

		defer release()

		return copyDir(src, dst, info, ancestors, pol)
	}

	if !pol.copyOnly {
		if err := os.Link(src, dst); err == nil {
			return nil
		}
	}

	// Not hard-linked. Refuse to overwrite an existing destination: a
	// truncating copy over a file already hard-linked elsewhere would corrupt
	// the shared inode. A genuinely absent dst means the link was unavailable
	// (e.g. cross-device) or not allowed, so make a byte copy.
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("copy %s: %w", dst, errDestExists)
	}

	return copyFileContents(src, dst, info)
}

func copyDir(src, dst string, info os.FileInfo, ancestors map[dirID]struct{}, pol copyPolicy) error {
	// Create the destination writable regardless of the source's mode, then
	// restore the source mode after populating: a read-only source directory
	// (e.g. 0555) must still admit its children being written into the copy.
	if err := os.MkdirAll(dst, dirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", src, err)
	}

	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), ancestors, pol); err != nil {
			return err
		}
	}

	// Restoring the source mode is best-effort: the exact directory permission is
	// a nicety, not a contract (publish names every leaf and reads by path), and a
	// filesystem that rejects the chmod would otherwise fail a copy that the prior
	// MkdirAll-with-source-mode tolerated. A more-permissive dirPerm is safe.
	_ = os.Chmod(dst, info.Mode().Perm())

	return nil
}

// closeChecked closes c and wraps a close(2) failure with name, returning nil on
// a clean close. Call it on a write function's happy path (`return closeChecked(
// f, name)`) so a close error propagates — the object-store (s3://) and NFS work
// dirs this data plane targets report write-back failures at close, not write, so
// a swallowed close would ship a truncated file that exits 0 and gets
// -resume-cached.
func closeChecked(c io.Closer, name string) error {
	if err := c.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}

	return nil
}

func copyFileContents(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	// The deferred close is the error-path safety net; the happy path closes via
	// CloseChecked so a close(2) failure propagates (closed guards a double close).
	closed := false
	defer func() {
		if !closed {
			_ = out.Close()
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}

	closed = true

	return closeChecked(out, dst)
}
