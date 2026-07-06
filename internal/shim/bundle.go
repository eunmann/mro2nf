package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// errDestExists is returned by CopyTree when the destination already exists, to
// avoid truncating a possibly hard-linked source.
var errDestExists = errors.New("destination already exists")

// errSymlinkCycle is returned by CopyTree when a directory output reaches one of
// its own ancestors through a symlink, which would otherwise recurse unbounded.
var errSymlinkCycle = errors.New("directory symlink cycle")

// The bundle is the object-store-portable channel item exchanged between
// Nextflow processes: a directory holding a JSON payload plus the actual files
// it references, so Nextflow stages files (not bare absolute paths) across task
// boundaries. File leaves in the payload are stored as markers; the real files
// live under the files subdir, named collision-free.
const (
	// FileMarker prefixes a plain-file leaf value in a bundle's payload. The
	// remainder is the bundle-relative path to the staged file. The prefix is
	// distinctive enough that no real data value collides with it.
	FileMarker = "@mre:file:"
	// DirMarker is FileMarker's counterpart for a leaf staged from a directory
	// (a Martian `path` output, or a directory a stage wrote into a `file`-typed
	// out). Both markers resolve to a path identically; the distinction records
	// the leaf's ground-truth dir-ness — measured by stat at staging time — so a
	// downstream consumer (publish's manifest) need not infer it from the declared
	// output type. See MarkFiles and cmd/mre/publish.go emitFile.
	DirMarker = "@mre:dir:"
	// bundleData is the payload filename inside a bundle directory.
	bundleData = "data.json"
	// bundleFiles is the files subdirectory inside a bundle directory.
	bundleFiles = "f"
)

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
// a marker (a plain scalar, or a raw path for a declared output never written).
func CutMarker(s string) (string, bool, bool) {
	if rel, ok := strings.CutPrefix(s, DirMarker); ok {
		return rel, true, true
	}

	if rel, ok := strings.CutPrefix(s, FileMarker); ok {
		return rel, false, true
	}

	return "", false, false
}

// ReadBundle loads a bundle directory's payload and rewrites every file marker
// to an absolute path under the (staged) bundle, so callers see real paths. An
// empty dir yields nil (an absent optional input). The resolution is
// type-agnostic: any string carrying the marker prefix is a file leaf.
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

	out, err := json.Marshal(resolveMarkers(v, abs))
	if err != nil {
		return nil, fmt.Errorf("encode bundle %s: %w", dir, err)
	}

	return out, nil
}

// decodeJSON decodes JSON into an any tree, keeping numbers as json.Number so a
// whole-number float like 42.0 is not silently rewritten to 42 on re-encode.
func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}

	return v, nil
}

// resolveMarkers walks decoded JSON, replacing marker strings with absolute
// paths under bundleAbs.
func resolveMarkers(v any, bundleAbs string) any {
	switch t := v.(type) {
	case string:
		if rel, _, ok := CutMarker(t); ok {
			return filepath.Join(bundleAbs, rel)
		}

		return t
	case []any:
		for i, e := range t {
			t[i] = resolveMarkers(e, bundleAbs)
		}

		return t
	case map[string]any:
		for k, e := range t {
			t[k] = resolveMarkers(e, bundleAbs)
		}

		return t
	default:
		return v
	}
}

// WriteBundle writes payload into dir as a bundle: every file leaf located by
// params/tbl is copied into dir/f/ and replaced with a marker, then the
// rewritten payload is written to dir/data.json.
func WriteBundle(dir string, payload map[string]any, params []ir.Param, tbl *types.Table) error {
	marked, err := MarkFiles(dir, payload, params, tbl)
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

// WriteChunkBundle writes a chunk definition as a bundle: the chunk's args have
// their file leaves (located via chunkIn) staged into the bundle, while the
// resource overrides are preserved verbatim for the scheduler to read.
func WriteChunkBundle(dir string, def ChunkDef, chunkIn []ir.Param, tbl *types.Table) error {
	argsMap := make(map[string]any, len(def.Args))
	for k, v := range def.Args {
		dv, err := decodeJSON(v)
		if err != nil {
			return fmt.Errorf("decode chunk arg %s: %w", k, err)
		}

		argsMap[k] = dv
	}

	marked, err := MarkFiles(dir, argsMap, chunkIn, tbl)
	if err != nil {
		return err
	}

	return writePayload(dir, map[string]any{"args": marked, "resources": def.Resources})
}

// MarkFiles copies every file leaf in payload (located via params/tbl) into
// dir/f/, replacing each with a bundle-relative marker, and returns the
// rewritten payload. Names are sequenced so distinct leaves never collide.
func MarkFiles(dir string, payload map[string]any, params []ir.Param, tbl *types.Table) (map[string]any, error) {
	n := 0

	copyIn := func(src string) (string, error) {
		if src == "" {
			return src, nil
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
			return src, nil
		}

		isDir := err == nil && info.IsDir()

		// Each leaf is staged under a flat, ordinal name (f/L0000, f/L0001, …) in
		// canonical walk order. The transport basename is not load-bearing — publish
		// names every leaf from the manifest via OutFilename, and the adapter reads
		// inputs by absolute path — so leaves are renumbered rather than carrying
		// their original basename through every hop. This flat, predictable naming is
		// what lets the leaves be staged as individual path items (epic #18 / #13).
		rel := filepath.Join(bundleFiles, fmt.Sprintf("L%04d", n))
		n++

		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
			return "", fmt.Errorf("create files dir: %w", err)
		}

		if err := CopyTree(src, dst); err != nil {
			return "", err
		}

		return Marker(rel, isDir), nil
	}

	marked, err := tbl.Apply(params, payload, copyIn)
	if err != nil {
		return nil, fmt.Errorf("collect bundle files: %w", err)
	}

	return marked, nil
}

// CopyTree links or copies src to dst, recursing into directories (Martian file
// outputs may be directories). It hard-links files when possible (same device)
// for speed, falling back to a byte copy.
func CopyTree(src, dst string) error {
	return copyTree(src, dst, nil)
}

// copyTree implements CopyTree, threading the set of resolved directory paths
// already on the recursion stack. A directory output that (via a symlink) points
// at one of its own ancestors would otherwise recurse until ENAMETOOLONG,
// duplicating the tree on every pass; catching the repeat real path bounds the
// recursion to the finite number of distinct directories reachable once.
func copyTree(src, dst string, ancestors map[string]struct{}) error {
	// Resolve a symlinked source to its real target first. Nextflow stages inputs
	// as symlinks, and link(2) does not follow them — hard-linking the symlink
	// would leave a dangling link once the bundle is staged into another isolated
	// task (AWS Batch + S3 / HealthOmics), where the original target is absent.
	if lst, err := os.Lstat(src); err == nil && lst.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", src, err)
		}

		src = resolved
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	if info.IsDir() {
		realDir, err := filepath.EvalSymlinks(src)
		if err != nil {
			return fmt.Errorf("resolve dir %s: %w", src, err)
		}

		if _, cycle := ancestors[realDir]; cycle {
			return fmt.Errorf("copy %s: %w", src, errSymlinkCycle)
		}

		if ancestors == nil {
			ancestors = make(map[string]struct{})
		}

		ancestors[realDir] = struct{}{}
		defer delete(ancestors, realDir)

		return copyDir(src, dst, info, ancestors)
	}

	if err := os.Link(src, dst); err == nil {
		return nil
	}

	// Hard-linking failed. Refuse to overwrite an existing destination: a
	// truncating copy over a file already hard-linked elsewhere would corrupt
	// the shared inode. A genuinely absent dst means the link was unavailable
	// (e.g. cross-device), so fall back to a byte copy.
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("copy %s: %w", dst, errDestExists)
	}

	return copyFileContents(src, dst, info)
}

func copyDir(src, dst string, info os.FileInfo, ancestors map[string]struct{}) error {
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
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), ancestors); err != nil {
			return err
		}
	}

	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
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

	// Close on the happy path explicitly and propagate the error. The object-store
	// (s3://) and NFS work dirs this data plane targets report write-back failures
	// — ENOSPC, EDQUOT, a failed upload — at close(2), not write(2). Swallowing the
	// close would return nil for a truncated leaf, which mre records in data.json,
	// exits 0, and every -resume then trusts: a silent divergence. The deferred
	// close is only the error-path safety net (closed guards the double close).
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
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}

	return nil
}
