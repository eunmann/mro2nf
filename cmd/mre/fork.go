package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// errForkIndexRange reports a native-scatter fork index past the resolved forks.
var errForkIndexRange = errors.New("forkbind -index out of range")

// errForkIndexNoOut reports -index without the required -o output dir.
var errForkIndexNoOut = errors.New("forkbind -index requires -o <dir>")

// errForkIndexNegative reports a -index below the -1 "unset" sentinel. Any other
// negative value would otherwise be treated as the full-collection write and
// silently drop -o, writing to -chunkdir instead of the requested output.
var errForkIndexNegative = errors.New("forkbind -index must be >= 0 (or -1 for the full write)")

// errKeysFileNeedsIndex reports -keysfile without -index or -keysonly (the
// full-fork write already emits forkmeta2.json into -chunkdir).
var errKeysFileNeedsIndex = errors.New("forkbind -keysfile requires -index or -keysonly")

// errKeysOnlyNeedsFile reports -keysonly without the -keysfile destination.
var errKeysOnlyNeedsFile = errors.New("forkbind -keysonly requires -keysfile <path>")

// errKeysOnlyWithIndex reports -keysonly combined with -index: the flags are
// mutually exclusive modes (keys-only would silently skip the -o bundle write).
var errKeysOnlyWithIndex = errors.New("forkbind -keysonly cannot be combined with -index")

// errElementNoOut reports -elementfile without the required -o output dir.
var errElementNoOut = errors.New("forkbind -elementfile requires -o <dir>")

// errElementConflict reports -elementfile combined with a full-collection flag
// (-index/-keysfile/-keysonly/-mapmode): the pre-sliced element path never
// indexes, writes keys, or dispatches on the fork kind, so such a flag would
// be silently ignored.
var errElementConflict = errors.New("forkbind -elementfile cannot be combined with -index, -keysfile, -keysonly, or -mapmode")

// runForkBind resolves a map call's bindings into one args file per fork,
// written as fork_NNNNN.json into -chunkdir so a lexical sort recovers order.
func runForkBind(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("forkbind", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleIn)
	specFile := fs.String("spec", "", "binding spec JSON file")
	pipeFile := fs.String("pipeargs", "", "enclosing pipeline args bundle dir")
	inputs := fs.String("inputs", "", "comma-separated callId=bundleDir pairs")
	dir := fs.String("chunkdir", ".", "directory to write per-fork args bundles")
	mapMode := fs.String("mapmode", "array", "static fork kind: 'map' (typed map) or 'array'")
	index := fs.Int("index", -1, "with -o, resolve and write ONLY this fork's args bundle (native-map scatter, #76)")
	oDir := fs.String("o", "", "output args bundle dir when -index >= 0")
	keysFile := fs.String("keysfile", "", "with -index or -keysonly, write the forkmeta sidecar to this path")
	keysOnly := fs.Bool("keysonly", false, "resolve and validate every binding but write ONLY the -keysfile sidecar (native scatter's empty-fork instance, #76)")
	elemFile := fs.String("elementfile", "", "with -o, assemble this fork's args from a PRE-SLICED split element (O(1), no whole-collection parse; native scatter #99)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if err := checkForkBindFlags(fs, *index, *keysOnly, *keysFile, *elemFile); err != nil {
		return err
	}

	spec, err := readSpec(*specFile)
	if err != nil {
		return err
	}

	pipeArgs, err := readBundle(*pipeFile)
	if err != nil {
		return err
	}

	callOuts, err := readInputs(*inputs)
	if err != nil {
		return err
	}

	nulls, err := readStaticNulls(*pipeFile, *inputs)
	if err != nil {
		return err
	}

	// -elementfile is the O(1) native-scatter path (#99): the driver sliced the
	// fork collection once and handed this instance only its element, so the args
	// are assembled without re-parsing the whole collection (the O(N^2) that the
	// per-fork -index path did across N instances).
	if *elemFile != "" {
		return writeForkElement(spec, pipeArgs, callOuts, nulls, *elemFile, *oDir, prod)
	}

	return resolveAndWriteForks(spec, pipeArgs, callOuts, prod, forkWrite{
		mapMode: *mapMode, index: *index, oDir: *oDir, dir: *dir,
		keysFile: *keysFile, keysOnly: *keysOnly, nulls: nulls,
	})
}

// forkWrite bundles the destination flags for the full-collection forkbind
// modes (all-forks / -index / -keysonly), and the static nulls of its inputs.
type forkWrite struct {
	mapMode             string
	index               int
	oDir, dir, keysFile string
	keysOnly            bool
	nulls               bind.StaticNulls
}

// resolveAndWriteForks resolves the whole fork collection and writes the
// requested output: -keysonly (validate + keys sidecar), -index (one fork's
// args + its keys), or the default full-fork write.
func resolveAndWriteForks(spec bind.Spec, pipeArgs json.RawMessage, callOuts map[string]json.RawMessage, prod *producer, w forkWrite) error {
	// A single-fork write (-index) marshals only that fork's args; validation
	// (split kind vs -mapmode, zip lengths, every binding's resolution) is
	// identical to the full resolve either way.
	only := bind.AllForks
	if w.index >= 0 {
		only = w.index
	}

	forks, keys, err := bind.ResolveForks(spec, pipeArgs, callOuts, w.mapMode == "map", only)
	if err != nil {
		return fmt.Errorf("forkbind: %w", err)
	}

	meta := forkMeta{Count: len(forks), Keys: keys, Pruned: bind.SplitOfStaticNull(spec, w.nulls, callOuts)}

	// -keysonly is the native scatter's empty-collection instance (#76): the
	// resolve above validated every binding exactly as the FORK task would (a
	// wrong-kind or mis-zipped source still fails loudly), and only the keys
	// sidecar is written — there is no fork to run.
	if w.keysOnly {
		return writeKeysFile(w.keysFile, meta)
	}

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	params, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("forkbind params: %w", err)
	}

	tbl := man.Table()

	// Native scatter (#76): -index writes just one fork's args to -o, so the
	// standalone FORK task is replaced by an in-workflow scatter over 0..N-1 with
	// each stage resolving its own fork inline. The per-fork args are identical
	// to the corresponding full-fork write, and -keysfile emits the same
	// forkmeta sidecar the full write would, so the gather still gets its keys.
	bound := forkBinding{
		spec: spec, meta: meta, params: params, tbl: tbl,
		nulls: bind.StaticNullParams(spec, w.nulls, callOuts),
	}
	if w.index >= 0 {
		return writeForkIndex(forks, w.index, w.oDir, w.keysFile, bound)
	}

	return writeAllForks(forks, w.dir, bound)
}

// forkBinding is what writes a resolved fork's args bundle: the call's spec,
// the forkmeta (whose keys are nil for an array fork), its params, and the
// broadcast params bound to a static null, which every fork's bundle records.
type forkBinding struct {
	spec   bind.Spec
	meta   forkMeta
	params []ir.Param
	tbl    *types.Table
	nulls  []string
}

// write writes fork index's resolved args as a bundle at dir.
func (b forkBinding) write(dir string, args json.RawMessage, index int) error {
	payload, err := rawToMap(args)
	if err != nil {
		return err
	}

	key := ""
	if b.meta.Keys != nil {
		key = b.meta.Keys[index]
	}

	if err := writeBoundBundle(dir, payload, bind.ForkSpec(b.spec, index, key), b.params, b.tbl); err != nil {
		return err
	}

	return writeStaticNulls(dir, b.nulls)
}

// writeForkElement assembles one fork's args bundle from a pre-sliced split
// element (native scatter #99): O(1) per instance, no whole-collection parse.
// The bundle is byte-identical to the corresponding -index write (same
// ResolveElement result, same WriteBundle) — the element the driver sliced is
// the same value ResolveForks would have placed at that fork's split param.
func writeForkElement(spec bind.Spec, pipeArgs json.RawMessage, callOuts map[string]json.RawMessage, nulls bind.StaticNulls, elemFile, oDir string, prod *producer) error {
	if oDir == "" {
		return errElementNoOut
	}

	element, err := readElement(elemFile)
	if err != nil {
		return err
	}

	args, err := bind.ResolveElement(spec, pipeArgs, callOuts, element)
	if err != nil {
		return fmt.Errorf("forkbind element: %w", err)
	}

	if err := prod.writeBound(oDir, args, bind.ForkSpec(spec, -1, "")); err != nil {
		return err
	}

	return writeStaticNulls(oDir, bind.StaticNullParams(spec, nulls, callOuts))
}

// readElement reads a pre-sliced fork element. The driver cuts it verbatim out
// of a bundle's data.json, so its strings are still bundle-encoded and are
// restored here, as ReadBundle restores the -index path's collection.
func readElement(path string) (json.RawMessage, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, fmt.Errorf("read element file: %w", err)
	}

	v, err := shim.UnescapeValue(raw)
	if err != nil {
		return nil, fmt.Errorf("decode element file: %w", err)
	}

	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode element: %w", err)
	}

	return out, nil
}

// checkForkBindFlags diagnoses flag-combination misuse before any resolution
// work, so the error names the actual mistake rather than whatever I/O failed
// first. The element path checks EXPLICITLY-set flags (via fs.Visit), so
// -mapmode's default does not trip the conflict while a passed one does.
func checkForkBindFlags(fs *flag.FlagSet, index int, keysOnly bool, keysFile, elemFile string) error {
	// -1 is the "unset" sentinel (full-collection write); a specific fork is >= 0.
	// Reject anything below -1 loudly rather than folding it into the full write,
	// which would silently ignore -o.
	if index < bind.AllForks {
		return fmt.Errorf("%w: got %d", errForkIndexNegative, index)
	}

	if elemFile != "" {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

		for _, name := range []string{"index", "keysfile", "keysonly", "mapmode"} {
			if set[name] {
				return fmt.Errorf("%w: got -%s", errElementConflict, name)
			}
		}

		return nil
	}

	if keysFile != "" && index < 0 && !keysOnly {
		return errKeysFileNeedsIndex
	}

	if keysOnly && keysFile == "" {
		return errKeysOnlyNeedsFile
	}

	if keysOnly && index >= 0 {
		return errKeysOnlyWithIndex
	}

	return nil
}

// writeAllForks writes every fork's args bundle (fork_NNNNN/) into dir plus the
// forknames/forkmeta sidecars — the default (FORK task) write.
func writeAllForks(forks []json.RawMessage, dir string, bound forkBinding) error {
	names := make([]string, len(forks))

	for i, args := range forks {
		name := fmt.Sprintf("fork_%05d", i)
		names[i] = name

		if err := bound.write(filepath.Join(dir, name), args, i); err != nil {
			return fmt.Errorf("write fork bundle %s: %w", name, err)
		}
	}

	return writeForkMeta(dir, names, bound.meta)
}

// writeForkIndex writes only fork[index]'s args bundle to oDir (the native-scatter
// path); it is identical to the corresponding full-fork write. With keysFile it
// also writes the forkmeta sidecar (identical to the full write's), so a scatter
// instance can supply the gather's keys without a FORK task.
func writeForkIndex(forks []json.RawMessage, index int, oDir, keysFile string, bound forkBinding) error {
	if oDir == "" {
		return errForkIndexNoOut
	}

	if index >= len(forks) {
		return fmt.Errorf("%w: index %d, %d forks", errForkIndexRange, index, len(forks))
	}

	if err := bound.write(oDir, forks[index], index); err != nil {
		return fmt.Errorf("write fork %d bundle: %w", index, err)
	}

	if keysFile == "" {
		return nil
	}

	return writeKeysFile(keysFile, bound.meta)
}

// writeKeysFile writes the forkmeta sidecar to path — byte-identical to the
// full-fork write's forkmeta2.json (same forkMetaJSON encoding).
func writeKeysFile(path string, meta forkMeta) error {
	raw, err := forkMetaJSON(meta)
	if err != nil {
		return err
	}

	return writeRaw(path, raw)
}

// writeForkMeta writes the two fork sidecar files into dir:
//   - forknames.json: the fork bundle dir names, so a keyed (nested-map) workflow
//     can enumerate forks without a java.io listFiles() (which cannot list an
//     object-store s3:// work dir).
//   - forkmeta2.json: the fork count and map keys (forkMeta), so merge can
//     check it gathered every fork and rebuild a keyed result.
func writeForkMeta(dir string, names []string, meta forkMeta) error {
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("marshal fork names: %w", err)
	}

	if err := writeRaw(filepath.Join(dir, "forknames.json"), namesJSON); err != nil {
		return err
	}

	return writeKeysFile(filepath.Join(dir, "forkmeta2.json"), meta)
}

// forkMeta is the forkmeta sidecar every fork producer writes and every merge
// reads. Count is the gather's source of truth for how many outs__* bundles
// it must find; an array fork has no keys of its own to count. Keys are the
// map fork's keys in fork-index order, nil (JSON null) for an array fork.
// Pruned marks a split of a statically null value (bind.SplitOfStaticNull),
// which merges zero forks to null; it is omitted when false, so every other
// sidecar keeps its bytes and -resume cache keys.
type forkMeta struct {
	Count  int      `json:"count"`
	Keys   []string `json:"keys"`
	Pruned bool     `json:"pruned,omitempty"`
}

// errForkMeta reports a forkmeta sidecar whose count and keys disagree.
var errForkMeta = errors.New("inconsistent forkmeta sidecar")

// forkMetaJSON renders the forkmeta sidecar payload.
func forkMetaJSON(meta forkMeta) (json.RawMessage, error) {
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal fork keys: %w", err)
	}

	return raw, nil
}

// forkOutPrefix names every per-fork output bundle a merge gathers: the
// emitter writes each fork's outs as outs__<fork key>.
const forkOutPrefix = "outs__"

// errNoKeysFile reports a merge without the forkmeta sidecar it counts against.
var errNoKeysFile = errors.New("merge requires -keysfile <forkmeta sidecar>")

// runMerge combines per-fork outputs into one map-call result: each named
// output becomes an ordered array across forks.
func runMerge(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleOut)
	outs := fs.String("outs", "", "comma-separated output parameter names")
	forksDir := fs.String("forksdir", ".", "directory holding the staged "+forkOutPrefix+"<key> per-fork output bundles")
	keysFile := fs.String("keysfile", "", "forkmeta sidecar: the fork count and map keys")
	outFile := fs.String("o", "", "output merged bundle dir")
	emptyNull := fs.Bool("emptynull", false, "zero forks merge to null instead of the typed empty (invocation-known split source, #99)")
	nullForks := fs.Bool("nullforks", false, "gather nothing: every fork in the keys sidecar is null (a map call over a sub-pipeline a runtime gate disables, #434)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	meta, err := readForkMeta(*keysFile)
	if err != nil {
		return err
	}

	names := splitComma(*outs)
	zeroNull := (*emptyNull || meta.Pruned) && meta.Count == 0

	merged, err := mergeForks(names, meta, *forksDir, zeroNull, *nullForks)
	if err != nil {
		return err
	}

	// The merge adds a fork dimension to every output (an array for an array
	// fork, a keyed map for a map fork), so the file-leaf walk must descend one
	// level deeper than the callee's declared output types or it will skip the
	// per-fork files and leave dangling paths under object-store staging.
	if err := writeMerged(*outFile, merged, prod, meta.Keys != nil); err != nil {
		return err
	}

	// A map call merged to null this way is the call mrp prunes statically, so
	// a split of it or of any of its outputs is pruned in turn.
	if zeroNull {
		return writeStaticNulls(*outFile, append(names, bind.StaticNullCall))
	}

	return nil
}

// mergeForks is the merged value of the forks meta counts: null outputs for
// a zero-fork prune (zeroNull), one null per fork for a disabled map call
// over a sub-pipeline (nullForks), else the staged outs__<key> bundles under
// forksDir merged.
func mergeForks(names []string, meta forkMeta, forksDir string, zeroNull, nullForks bool) (json.RawMessage, error) {
	if nullForks && !zeroNull {
		merged, err := bind.NullForks(names, meta.Count, meta.Keys)
		if err != nil {
			return nil, fmt.Errorf("merge: %w", err)
		}

		return merged, nil
	}

	dirs, err := gatherIndexed(forksDir, forkOutPrefix, meta.Count)
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}

	forkOuts, err := readBundleList(dirs)
	if err != nil {
		return nil, err
	}

	merged, err := bind.Merge(names, forkOuts, meta.Keys, zeroNull)
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}

	return merged, nil
}

// writeMerged writes the merged map-call result as a bundle, bumping each output
// param's outer dimension to match the fork shape so nested files are staged.
func writeMerged(dir string, merged json.RawMessage, prod *producer, mapFork bool) error {
	man, err := prod.manifest()
	if err != nil {
		return err
	}

	payload, err := rawToMap(merged)
	if err != nil {
		return err
	}

	outParams, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("merge params: %w", err)
	}

	params := bumpForkDim(outParams, mapFork)
	if err := shim.WriteBundle(dir, payload, params, man.Table(), nil); err != nil {
		return fmt.Errorf("write merged bundle %s: %w", dir, err)
	}

	return nil
}

// bumpForkDim raises each param's outer dimension by one to reflect the extra
// fork dimension the merge introduces.
func bumpForkDim(params []ir.Param, mapFork bool) []ir.Param {
	out := make([]ir.Param, len(params))
	copy(out, params)

	for i := range out {
		if mapFork {
			out[i].MapDim++
		} else {
			out[i].ArrayDim++
		}
	}

	return out
}

// readForkMeta reads and validates the forkmeta sidecar: a JSON null keys
// field (array fork) yields nil keys, an array the map fork's keys, which must
// number exactly the fork count.
func readForkMeta(path string) (forkMeta, error) {
	if path == "" {
		return forkMeta{}, errNoKeysFile
	}

	data, err := readFile(path)
	if err != nil {
		return forkMeta{}, err
	}

	// A pointer count tells an absent field from a zero-fork sidecar.
	var raw struct {
		Count  *int     `json:"count"`
		Keys   []string `json:"keys"`
		Pruned bool     `json:"pruned"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return forkMeta{}, fmt.Errorf("parse fork keys %s: %w", path, err)
	}

	if raw.Count == nil {
		return forkMeta{}, fmt.Errorf("%w %s: no count", errForkMeta, path)
	}

	meta := forkMeta{Count: *raw.Count, Keys: raw.Keys, Pruned: raw.Pruned}
	if meta.Count < 0 || (meta.Keys != nil && len(meta.Keys) != meta.Count) {
		return forkMeta{}, fmt.Errorf("%w %s: count %d, %d keys", errForkMeta, path, meta.Count, len(meta.Keys))
	}

	return meta, nil
}
