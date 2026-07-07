package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// runPublishLayout computes the outs/ layout from the final output sidecar ALONE
// (no file leaves staged, so no single-node funnel — #12): it walks the raw
// marker-bearing sidecar and writes layout.json (transport leaf basename -> outs/
// rel path) plus the pipeline_outs.json value tree. A fan-out then publishes each
// leaf into outs/<rel> in parallel.
func runPublishLayout(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("publish-layout", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleOut)
	sidecar := fs.String("sidecar", "", "raw output sidecar (data.json, markers intact)")
	dir := fs.String("dir", ".", "directory to write layout.json and pipeline_outs.json")
	leavesDir := fs.String("leaves", "", "dir of staged leaf files (head-node publish); empty leaves publishing off")
	outsDir := fs.String("outs", "", "dir to materialise the outs/ tree into for head-node publishDir")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	raw, err := readFile(*sidecar)
	if err != nil {
		return err
	}

	outs, err := rawToMap(raw)
	if err != nil {
		return err
	}

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	pub := newPublisher(man.Structs)

	params, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("publish params: %w", err)
	}

	published, err := pub.publishOuts(params, outs)
	if err != nil {
		return err
	}

	if err := writeJSON(filepath.Join(*dir, "layout.json"), pub.layout); err != nil {
		return err
	}

	if err := writeManifest(filepath.Join(*dir, "manifest.json.gz"), prod.callable, pub.manifest); err != nil {
		return err
	}

	out, err := json.MarshalIndent(published, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal published outs: %w", err)
	}

	if err := writeRaw(filepath.Join(*dir, "pipeline_outs.json"), out); err != nil {
		return err
	}

	// Head-node publish (POSIX targets): materialise the outs/ tree from the
	// staged leaves so a single publishDir directive on LAYOUT copies it into the
	// results tree — no per-leaf PUBLISH_LEAF task. Off (both flags empty) on
	// object-store backends, which keep the parallel fan-out.
	if *outsDir != "" {
		return materializeOuts(*leavesDir, filepath.Join(*dir, *outsDir), pub.layout)
	}

	return nil
}

// materializeOuts links each staged leaf into its published outs/ rel path(s),
// reproducing the exact tree publish-layout computed. A leaf referenced by
// several outputs is linked into each rel; CopyTree hard-links files (recursing
// directory leaves) so the tree is built without copying bytes, and a later
// publishDir copies it into the results dir. A layout key with no matching
// staged leaf is skipped — the leaf was absent at staging time and resolves to
// null, exactly as the sidecar already recorded.
// outsDirPerm is the mode for outs/ parent directories; publishDir copies the
// tree out, so this is transient scratch, not a published permission.
const outsDirPerm = 0o750

func materializeOuts(leavesDir, outsDir string, layout map[string][]string) error {
	for base, rels := range layout {
		src := filepath.Join(leavesDir, base)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return fmt.Errorf("stat leaf %q: %w", base, err)
		}

		for _, rel := range rels {
			dst := filepath.Join(outsDir, rel)
			if err := os.MkdirAll(filepath.Dir(dst), outsDirPerm); err != nil {
				return fmt.Errorf("create outs dir for %q: %w", rel, err)
			}

			if err := shim.CopyTree(src, dst); err != nil {
				return fmt.Errorf("materialise outs %q: %w", rel, err)
			}
		}
	}

	return nil
}

// manifestSchemaVersion is bumped only on a breaking manifest change; consumers
// ignore unknown fields.
const manifestSchemaVersion = 1

// writeManifest writes the gzip-compressed output manifest: a flat, versioned
// index a downstream control plane ingests in one GetObject (no S3 LIST).
func writeManifest(path, pipeline string, outputs []manifestEntry) error {
	if outputs == nil {
		outputs = []manifestEntry{}
	}

	data, err := json.Marshal(struct {
		SchemaVersion int             `json:"schema_version"`
		Pipeline      string          `json:"pipeline"`
		Outputs       []manifestEntry `json:"outputs"`
	}{manifestSchemaVersion, pipeline, outputs})
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}

	// The deferred close is the error-path safety net; the happy path closes via
	// CloseChecked so a close(2) write-back failure (which the object-store/NFS
	// work dirs report at close, not write) propagates instead of shipping a
	// truncated manifest that exits 0.
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	gz := gzip.NewWriter(f)
	if _, err := gz.Write(data); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	if err := gz.Close(); err != nil {
		return fmt.Errorf("close manifest gzip: %w", err)
	}

	closed = true
	if err := f.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}

	return nil
}

// publishOuts walks each declared output param and emits its value, returning the
// rewritten outs tree (file leaves replaced by their outs/ rel path, or null).
// Non-file values pass through unchanged; a missing/empty file leaf resolves to
// null. Keys without a matching declared output param are preserved.
func (pub *publisher) publishOuts(params []ir.Param, outs map[string]any) (map[string]any, error) {
	published := maps.Clone(outs)

	for _, p := range params {
		if v, ok := outs[p.Name]; ok {
			ev, err := pub.emit("", p, v)
			if err != nil {
				return nil, err
			}

			published[p.Name] = ev
		}
	}

	return published, nil
}

// publisher computes a pipeline's mrp-style outs/ layout from the raw sidecar
// markers WITHOUT copying files, so a distributed fan-out can publish each leaf
// into place (no single-node funnel — #12).
type publisher struct {
	structs map[string]*ir.StructType
	// warn receives per-skipped-key diagnostics (illegal map keys, #114) —
	// os.Stderr in production, a buffer in tests.
	warn io.Writer
	// seen maps a published rel path to the transport basename it resolved from,
	// so a repeated leaf dedups while two distinct leaves on one rel disambiguate.
	seen map[string]string
	// layout maps a transport leaf basename to the outs/ rel path(s) it publishes
	// to — a list, since one file value may be referenced by several outputs.
	layout map[string][]string
	// manifest accumulates one entry per published file leaf for the
	// machine-readable output index (see manifestEntry).
	manifest []manifestEntry
}

// newPublisher returns a publisher over the program's struct table.
func newPublisher(structs map[string]*ir.StructType) *publisher {
	return &publisher{structs: structs, warn: os.Stderr, seen: map[string]string{}, layout: map[string][]string{}}
}

// manifestEntry is one published output file in the manifest index: a downstream
// control plane reads the manifest once (no S3 LIST) to catalog outputs.
type manifestEntry struct {
	Path     string `json:"path"`
	BaseType string `json:"base_type"`
	IsDir    bool   `json:"is_dir"`
}

// emit publishes value (of param p's type) under parentRel, naming this node by
// GetOutFilename(p). It records file leaves in the layout/manifest and returns
// the JSON value: the outs/ rel path for a file, a nested array/object for a
// collection/struct, the unchanged value for a non-file scalar, or nil for a
// null/absent leaf. Nothing touches the filesystem. A file-bearing value whose
// runtime shape contradicts its declared type is an error, not a silent
// pass-through (which would leak a transport marker into pipeline_outs.json).
func (pub *publisher) emit(parentRel string, p ir.Param, value any) (any, error) {
	if value == nil {
		return nil, nil
	}

	// A type with no file/dir leaves (a plain scalar, or an array/map/struct of
	// them) is passed through verbatim — matching Martian's non-file outs, which
	// are emitted unchanged (all map keys and struct fields kept). Only file-
	// bearing values are decomposed into the outs/ tree. IsFile is set for a file,
	// a directory, and any array/map/struct that contains one.
	if !p.IsFile {
		return value, nil
	}

	rel := path.Join(parentRel, types.OutFilename(p, pub.isStruct))

	switch {
	case p.ArrayDim > 0:
		return pub.emitArray(rel, p, value)
	case p.MapDim > 0:
		return pub.emitMap(rel, p, value)
	case pub.isStruct(p.BaseType):
		return pub.emitStruct(rel, p, value)
	default:
		return pub.emitFile(rel, p, value)
	}
}

// errShapeMismatch flags a file-bearing output whose runtime value contradicts
// its declared container type. Returning the value verbatim (the old behavior)
// would let a raw @mre:file: transport marker reach pipeline_outs.json and skip
// the layout/manifest entirely — unreachable from a well-formed binder, so this
// guards a binder shape bug or a hand-edited/forked sidecar.
var errShapeMismatch = errors.New("publish output shape mismatch")

// shapeErr wraps errShapeMismatch with the offending output's name, outs/ rel,
// declared container, and the actual runtime value's type.
func shapeErr(rel, want string, p ir.Param, value any) error {
	return fmt.Errorf("%w: %q at %s declared %s but sidecar value is %T", errShapeMismatch, p.Name, rel, want, value)
}

// isStruct reports whether name is one of the pipeline's struct types.
func (pub *publisher) isStruct(name string) bool {
	return pub.structs[name] != nil
}

// emitArray publishes an array into the rel/ subdir, naming elements by a
// zero-padded index (width = digits of the element count, matching Martian's
// WidthForInt) plus the element type's own GetOutFilename suffix.
func (pub *publisher) emitArray(rel string, p ir.Param, value any) (any, error) {
	arr, ok := value.([]any)
	if !ok {
		return nil, shapeErr(rel, "array", p, value)
	}

	width := len(strconv.Itoa(len(arr)))
	out := make([]any, len(arr))

	for i, ev := range arr {
		elem := p
		elem.ArrayDim--
		elem.Name = fmt.Sprintf("%0*d", width, i)
		elem.OutName = ""

		ov, err := pub.emit(rel, elem, ev)
		if err != nil {
			return nil, err
		}

		out[i] = ov
	}

	return out, nil
}

// emitMap publishes a typed map into the rel/ subdir, naming elements by their
// (legal, sorted) keys. A key that is not a legal Unix filename is dropped from
// the tree AND the outs JSON with a stderr warning — matching Martian, which
// drops such keys from the rewritten outs with a printed error (post_process.go
// moveOutFiles, TypedMapType case). A typed map is exactly one map level whose
// value carries MapDim-1 inner array dims (Martian's encoding: map<T[]> is
// {MapDim:2, ArrayDim:0}), so the element is descended as an array of that
// depth — not another map level.
func (pub *publisher) emitMap(rel string, p ir.Param, value any) (any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, shapeErr(rel, "map", p, value)
	}

	out := make(map[string]any, len(m))

	for _, k := range slices.Sorted(maps.Keys(m)) {
		if err := legalFilenameErr(k); err != nil {
			// Best-effort diagnostic write: a failed stderr warning must not
			// fail the publish itself.
			_, _ = fmt.Fprintf(pub.warn, "mre: publish: skipping map key %q of output %q: %v\n", k, rel, err)

			continue
		}

		elem := p
		elem.ArrayDim = p.ArrayDim + p.MapDim - 1
		elem.MapDim = 0
		elem.Name = k
		elem.OutName = ""

		ov, err := pub.emit(rel, elem, m[k])
		if err != nil {
			return nil, err
		}

		out[k] = ov
	}

	return out, nil
}

// emitStruct publishes a struct into the rel/ subdir, recursing each field named
// by its own GetOutFilename; the JSON object is keyed by field id.
func (pub *publisher) emitStruct(rel string, p ir.Param, value any) (any, error) {
	sv, ok := value.(map[string]any)
	if !ok {
		return nil, shapeErr(rel, "struct", p, value)
	}

	out := make(map[string]any, len(pub.structs[p.BaseType].Fields))

	for _, f := range pub.structs[p.BaseType].Fields {
		// Every declared member is emitted (an absent one as null), matching
		// Martian; a non-file field passes through verbatim via emit's guard.
		fv, err := pub.emit(rel, f, sv[f.Name])
		if err != nil {
			return nil, err
		}

		out[f.Name] = fv
	}

	return out, nil
}

// emitFile records one scalar file/dir leaf's transport basename -> rel mapping
// and its manifest entry, and returns rel — the ACTUAL published path, which may
// differ from the argument after collision disambiguation. It returns nil when
// the leaf is absent (matching Martian's null): a present leaf carries a
// @mre:file:/@mre:dir: marker, while a declared output that was never written
// keeps a raw path and resolves to null. Nothing is copied.
func (pub *publisher) emitFile(rel string, p ir.Param, value any) (any, error) {
	src, ok := value.(string)
	if !ok {
		// A scalar file/dir leaf is always a path string or null in a well-formed
		// sidecar; a non-string here is a shape bug (an array/object where a leaf
		// belongs), reported rather than silently nulled.
		return nil, shapeErr(rel, "file", p, value)
	}

	transport, isDir, ok := shim.CutMarker(src)
	if !ok {
		// A non-marker string — an empty string, or a declared output the stage
		// never wrote (a raw, marker-less path) — resolves to null, matching
		// Martian.
		return nil, nil
	}

	base := path.Base(transport)
	// The SAME leaf landing on the same rel twice (one value referenced by two
	// identically-named outputs) publishes once; two DISTINCT leaves colliding on
	// one rel (e.g. two outputs sharing an explicit OutName) are disambiguated
	// with a numeric suffix so neither file is silently mapped over the other.
	if prev, ok := pub.seen[rel]; ok {
		if prev == base {
			return rel, nil
		}

		rel = pub.uniqueRel(rel)
	}

	pub.seen[rel] = base
	pub.layout[base] = append(pub.layout[base], rel)
	// One manifest entry per published leaf. is_dir is the leaf's ground truth —
	// the stat recorded in the marker at staging time — not an inference from the
	// declared type, so a directory written into a `file`-typed out (or a file
	// into a `path`-typed out) is catalogued correctly.
	pub.manifest = append(pub.manifest, manifestEntry{Path: rel, BaseType: p.BaseType, IsDir: isDir})

	return rel, nil
}

// uniqueRel returns rel, or rel with a numeric suffix before its extension when
// rel is already taken by a different source, so distinct colliding leaves keep
// distinct published paths.
func (pub *publisher) uniqueRel(rel string) string {
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)

	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if _, ok := pub.seen[cand]; !ok {
			return cand
		}
	}
}

// The reasons a map key is illegal as a filename, in Martian's
// IsLegalUnixFilename wording so mre's warning reads like mrp's error.
var (
	errNameTooLong  = errors.New("too long")
	errNameEmpty    = errors.New("empty string")
	errNameReserved = errors.New("reserved name")
	errNameSlash    = errors.New("'/' is not allowed in filenames")
	errNameNul      = errors.New("null characters are not allowed in filenames")
)

// legalFilenameErr mirrors Martian's syntax.IsLegalUnixFilename: nil for a
// non-empty single path segment of at most 255 bytes that is not "." or "..",
// otherwise the reason the name is illegal. Publishing under an illegal key
// would create an invalid path (or ENAMETOOLONG), so the caller skips it like
// Martian does. The rune checks are a single left-to-right scan, matching
// Martian's iteration, so the reported reason is that of the FIRST offending
// rune (e.g. "a\x00/b" is a null-character error, not a '/' error).
func legalFilenameErr(k string) error {
	const maxNameLen = 255

	switch {
	case len(k) > maxNameLen:
		return errNameTooLong
	case k == "":
		return errNameEmpty
	case k == "." || k == "..":
		return errNameReserved
	}

	for _, c := range k {
		switch c {
		case '/':
			return errNameSlash
		case 0:
			return errNameNul
		}
	}

	return nil
}
