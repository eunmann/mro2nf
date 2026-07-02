package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
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

	published, err := pub.publishOuts(man.Params(prod.callable, prod.role), outs)
	if err != nil {
		return fmt.Errorf("compute layout: %w", err)
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

	return writeRaw(filepath.Join(*dir, "pipeline_outs.json"), out)
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
	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	if _, err := gz.Write(data); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	if err := gz.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}

	return nil
}

// publishOuts walks each declared output param and emits its value, returning the
// rewritten outs tree (file leaves replaced by their outs/ rel path, or null).
// Non-file values pass through unchanged; a missing/empty file leaf resolves to
// null. Keys without a matching declared output param are preserved.
func (pub *publisher) publishOuts(params []ir.Param, outs map[string]any) (map[string]any, error) {
	published := make(map[string]any, len(outs))
	maps.Copy(published, outs)

	for _, p := range params {
		v, ok := outs[p.Name]
		if !ok {
			continue
		}

		jv, err := pub.emit("", p, v)
		if err != nil {
			return nil, err
		}

		published[p.Name] = jv
	}

	return published, nil
}

// publisher computes a pipeline's mrp-style outs/ layout from the raw sidecar
// markers WITHOUT copying files, so a distributed fan-out can publish each leaf
// into place (no single-node funnel — #12).
type publisher struct {
	structs map[string]*ir.StructType
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
	return &publisher{structs: structs, seen: map[string]string{}, layout: map[string][]string{}}
}

// manifestEntry is one published output file in the manifest index: a downstream
// control plane reads the manifest once (no S3 LIST) to catalog outputs.
type manifestEntry struct {
	Path     string `json:"path"`
	BaseType string `json:"base_type"`
	IsDir    bool   `json:"is_dir"`
}

// emit publishes value (of param p's type) under parentRel, naming this node by
// GetOutFilename(p). It copies file leaves into dir and returns the JSON value:
// the path within dir for a file, a nested array/object for a collection/struct,
// the unchanged value for a non-file scalar, or nil for a null/absent leaf.
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
		return pub.emitFile(rel, p, value), nil
	}
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
		return value, nil
	}

	width := len(strconv.Itoa(len(arr)))
	out := make([]any, len(arr))

	for i, ev := range arr {
		elem := p
		elem.ArrayDim--
		elem.Name = fmt.Sprintf("%0*d", width, i)
		elem.OutName = ""

		jv, err := pub.emit(rel, elem, ev)
		if err != nil {
			return nil, err
		}

		out[i] = jv
	}

	return out, nil
}

// emitMap publishes a typed map into the rel/ subdir, naming elements by their
// (legal, sorted) keys. Illegal Unix filenames are skipped, matching Martian. A
// typed map is exactly one map level whose value carries MapDim-1 inner array
// dims (Martian's encoding: map<T[]> is {MapDim:2, ArrayDim:0}), so the element
// is descended as an array of that depth — not another map level.
func (pub *publisher) emitMap(rel string, p ir.Param, value any) (any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}

	out := make(map[string]any, len(m))

	for _, k := range legalSortedKeys(m) {
		elem := p
		elem.ArrayDim = p.ArrayDim + p.MapDim - 1
		elem.MapDim = 0
		elem.Name = k
		elem.OutName = ""

		jv, err := pub.emit(rel, elem, m[k])
		if err != nil {
			return nil, err
		}

		out[k] = jv
	}

	return out, nil
}

// emitStruct publishes a struct into the rel/ subdir, recursing each field named
// by its own GetOutFilename; the JSON object is keyed by field id.
func (pub *publisher) emitStruct(rel string, p ir.Param, value any) (any, error) {
	sv, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}

	out := make(map[string]any, len(pub.structs[p.BaseType].Fields))

	for _, f := range pub.structs[p.BaseType].Fields {
		// Every declared member is emitted (an absent one as null), matching
		// Martian; a non-file field passes through verbatim via emit's guard.
		jv, err := pub.emit(rel, f, sv[f.Name])
		if err != nil {
			return nil, err
		}

		out[f.Name] = jv
	}

	return out, nil
}

// emitFile records one scalar file/dir leaf's transport basename -> rel mapping
// and its manifest entry, and returns rel — the ACTUAL published path, which may
// differ from the argument after collision disambiguation. It returns nil when
// the leaf is absent (matching Martian's null): a present leaf carries a
// @mre:file: marker, while a declared output that was never written keeps a raw
// path and resolves to null. Nothing is copied.
func (pub *publisher) emitFile(rel string, p ir.Param, value any) any {
	src, ok := value.(string)
	if !ok || src == "" {
		return nil
	}

	marker, ok := strings.CutPrefix(src, shim.FileMarker)
	if !ok {
		return nil
	}

	base := path.Base(marker)
	// The SAME leaf landing on the same rel twice (one value referenced by two
	// identically-named outputs) publishes once; two DISTINCT leaves colliding on
	// one rel (e.g. two outputs sharing an explicit OutName) are disambiguated
	// with a numeric suffix so neither file is silently mapped over the other.
	if prev, ok := pub.seen[rel]; ok {
		if prev == base {
			return rel
		}

		rel = pub.uniqueRel(rel)
	}

	pub.seen[rel] = base
	pub.layout[base] = append(pub.layout[base], rel)
	// One manifest entry per published leaf. A `path`-typed leaf is a directory.
	pub.manifest = append(pub.manifest, manifestEntry{Path: rel, BaseType: p.BaseType, IsDir: p.BaseType == "path"})

	return rel
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

// legalSortedKeys returns m's keys in sorted order, dropping any that are not a
// legal single-segment Unix filename (Martian skips these with a printed error).
func legalSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if isLegalFilename(k) {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	return keys
}

// isLegalFilename mirrors Martian's IsLegalUnixFilename: a non-empty single path
// segment of at most 255 bytes, not "." or "..". Publishing under an illegal key
// would create an invalid path (or ENAMETOOLONG), so it is skipped like Martian.
func isLegalFilename(k string) bool {
	const maxNameLen = 255

	return k != "" && k != "." && k != ".." &&
		len(k) <= maxNameLen && !strings.ContainsAny(k, "/\x00")
}
