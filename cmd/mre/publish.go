package main

import (
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

// publishDirPerm is the mode for created outs/ subdirectories. A named constant
// keeps the published tree group/other-readable (downstream pipelines may run as
// a different user) without tripping gosec's octal-literal check.
const publishDirPerm = 0o755

// runPublish finalizes a pipeline's outputs. It reads the final output bundle
// (resolving every file leaf to a real path) and lays the file outputs out under
// -dir exactly as Martian's mrp does in a pipestance `outs/` tree — each output
// named by GetOutFilename, arrays/maps/structs nested into index/key/field-named
// subdirectories — so a downstream pipeline can consume the tree like an mrp one.
func runPublish(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleOut)
	bundleDir := fs.String("bundle", "", "final output bundle dir")
	dir := fs.String("dir", ".", "directory to copy files into and write outs")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	raw, err := readBundle(*bundleDir)
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

	published, err := publishOuts(*dir, man.Params(prod.callable, prod.role), man.Structs, outs)
	if err != nil {
		return fmt.Errorf("publish files: %w", err)
	}

	out, err := json.MarshalIndent(published, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal published outs: %w", err)
	}

	return writeRaw(filepath.Join(*dir, "pipeline_outs.json"), out)
}

// publishOuts copies the file leaves of outs into dir under the Martian outs/
// layout and returns the rewritten outs (file leaves replaced by their path
// within dir). Non-file values pass through unchanged; a missing/empty file leaf
// resolves to null. Keys without a matching declared output param are preserved.
func publishOuts(dir string, params []ir.Param, structs map[string]*ir.StructType, outs map[string]any) (map[string]any, error) {
	pub := &publisher{dir: dir, structs: structs, seen: map[string]bool{}}

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

// publisher lays out a pipeline's file outputs under dir like mrp's outs/ tree.
type publisher struct {
	dir     string
	structs map[string]*ir.StructType
	seen    map[string]bool
}

// emit publishes value (of param p's type) under parentRel, naming this node by
// GetOutFilename(p). It copies file leaves into dir and returns the JSON value:
// the path within dir for a file, a nested array/object for a collection/struct,
// the unchanged value for a non-file scalar, or nil for a null/absent leaf.
func (pub *publisher) emit(parentRel string, p ir.Param, value any) (any, error) {
	if value == nil {
		return nil, nil
	}

	rel := path.Join(parentRel, pub.outFilename(p))

	switch {
	case p.ArrayDim > 0:
		return pub.emitArray(rel, p, value)
	case p.MapDim > 0:
		return pub.emitMap(rel, p, value)
	case pub.structs[p.BaseType] != nil:
		return pub.emitStruct(rel, p, value)
	case p.IsFile:
		return pub.emitFile(rel, value)
	default:
		return value, nil
	}
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
// (legal, sorted) keys. Illegal Unix filenames are skipped, matching Martian.
func (pub *publisher) emitMap(rel string, p ir.Param, value any) (any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}

	out := make(map[string]any, len(m))

	for _, k := range legalSortedKeys(m) {
		elem := p
		elem.MapDim--
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

	out := make(map[string]any, len(sv))

	for _, f := range pub.structs[p.BaseType].Fields {
		fv, ok := sv[f.Name]
		if !ok {
			continue
		}

		jv, err := pub.emit(rel, f, fv)
		if err != nil {
			return nil, err
		}

		out[f.Name] = jv
	}

	return out, nil
}

// emitFile copies one scalar file/dir leaf to dir/rel and returns rel, or nil
// when the source is empty or was never written (matching Martian's null).
func (pub *publisher) emitFile(rel string, value any) (any, error) {
	src, ok := value.(string)
	if !ok || src == "" {
		return nil, nil
	}

	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return nil, nil
	}

	if !pub.seen[rel] {
		dst := filepath.Join(pub.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), publishDirPerm); err != nil {
			return nil, fmt.Errorf("publish %s: %w", rel, err)
		}

		if err := shim.CopyTree(src, dst); err != nil {
			return nil, fmt.Errorf("publish %s: %w", rel, err)
		}

		pub.seen[rel] = true
	}

	return rel, nil
}

// outFilename mirrors Martian's StructMember.GetOutFilename: an explicit OutName
// wins; a complex type (array/map/struct) or the builtin file/path types use the
// bare name; any other (user) file type appends .<typename>.
func (pub *publisher) outFilename(p ir.Param) string {
	switch {
	case p.OutName != "":
		return p.OutName
	case p.ArrayDim > 0 || p.MapDim > 0 || pub.structs[p.BaseType] != nil:
		return p.Name
	case p.BaseType == "file" || p.BaseType == "path":
		return p.Name
	default:
		return p.Name + "." + p.BaseType
	}
}

// legalSortedKeys returns m's keys in sorted order, dropping any that are not a
// legal single-segment Unix filename (Martian skips these with a printed error).
func legalSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" && k != "." && k != ".." && !strings.ContainsAny(k, "/\x00") {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	return keys
}
