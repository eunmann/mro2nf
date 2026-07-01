package shim

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// prepDirs creates the metadata and files directories for a phase and returns
// the absolute metadata dir, files dir, and journal prefix path the adapter
// writes to.
func prepDirs(workDir, phase string) (string, string, string, error) {
	meta := filepath.Join(workDir, phase)
	files := filepath.Join(meta, "files")

	if err := os.MkdirAll(files, dirPerm); err != nil {
		return "", "", "", fmt.Errorf("create work dirs: %w", err)
	}

	absMeta, err := filepath.Abs(meta)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve meta dir: %w", err)
	}

	absFiles, err := filepath.Abs(files)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve files dir: %w", err)
	}

	return absMeta, absFiles, filepath.Join(absMeta, "journal"), nil
}

// writeJobInfo writes the minimal _jobinfo the Martian adapter requires
// (_CachedJobInfo reads profile_mode, stackvars_flag, invocation, version,
// threads, memGB).
func writeJobInfo(meta, files, phase string, res Resources, inv Invocation) error {
	info := map[string]any{
		"name":           inv.Call + "." + phase,
		"type":           "local",
		"cwd":            files,
		"threads":        res.Threads,
		"memGB":          res.MemGB,
		"vmemGB":         res.VMemGB,
		"profile_mode":   disableFlag,
		"stackvars_flag": disableFlag,
		"monitor_flag":   disableFlag,
		"invocation": map[string]any{
			"call":     inv.Call,
			"args":     orEmptyObj(inv.Args),
			"mro_file": inv.MROFile,
		},
		"version": map[string]any{
			"martian":   "mro2nf",
			"pipelines": "mro2nf",
		},
	}

	return writeJSON(filepath.Join(meta, "_jobinfo"), info)
}

// writeSkeletonOuts writes the _outs skeleton the adapter reads before a stage
// runs, pre-populating it the way Martian's makeOutArg does (core/stage.go):
// array dims become [], map dims {}, a scalar file leaf its default writable
// path under files, and everything else (plain scalars, structs) null. Stages
// that write to or assert on a pre-populated output path rely on this.
func writeSkeletonOuts(meta, files string, outParams []ir.Param, tbl *types.Table) error {
	outs := make(map[string]any, len(outParams))
	for _, p := range outParams {
		outs[p.Name] = skeletonOut(p, files, tbl)
	}

	return writeJSON(filepath.Join(meta, "_outs"), outs)
}

// skeletonOut returns the pre-populated _outs value for one declared output,
// mirroring Martian's makeOutArg / GetOutFilename.
func skeletonOut(p ir.Param, files string, tbl *types.Table) any {
	switch {
	case p.ArrayDim > 0:
		return []any{}
	case p.MapDim > 0:
		return map[string]any{}
	case tbl.IsStruct(p.BaseType):
		// A struct (complex) output, including a struct-as-directory: null. The
		// stage populates it; pre-filling a path would not match Martian.
		return nil
	case p.IsFile:
		// A scalar file leaf: pre-fill its default writable path. The shared
		// OutFilename rule is the same one publish uses, so the stage writes to
		// exactly where publish later looks.
		return filepath.Join(files, types.OutFilename(p, tbl.IsStruct))
	default:
		return nil
	}
}

// readStageDefs parses a split phase's _stage_defs into chunk definitions and
// the optional join-phase resource override. The join override (Martian's
// `{"join": {...}}` key) sets the JOIN phase's resources, overlaying the stage's
// `using` block field-by-field — the same semantics as a per-chunk override.
func readStageDefs(meta string) ([]ChunkDef, Resources, error) {
	raw, err := readRaw(filepath.Join(meta, "_stage_defs"))
	if err != nil {
		return nil, Resources{}, err
	}

	var sd struct {
		Chunks []map[string]json.RawMessage `json:"chunks"`
		Join   map[string]json.RawMessage   `json:"join"`
	}

	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, Resources{}, fmt.Errorf("parse _stage_defs: %w", err)
	}

	defs := make([]ChunkDef, 0, len(sd.Chunks))
	for _, c := range sd.Chunks {
		defs = append(defs, splitChunk(c))
	}

	join, _ := parseResources(sd.Join)

	return defs, join, nil
}

func splitChunk(chunk map[string]json.RawMessage) ChunkDef {
	res, args := parseResources(chunk)

	return ChunkDef{Args: args, Resources: res}
}

// parseResources splits a Martian arg map into its __-prefixed resource keys and
// the remaining data args. Used for both per-chunk defs and the join override.
func parseResources(m map[string]json.RawMessage) (Resources, map[string]json.RawMessage) {
	var res Resources

	args := make(map[string]json.RawMessage, len(m))

	for key, val := range m {
		switch key {
		case "__threads":
			res.Threads = asFloat(val)
		case "__mem_gb":
			res.MemGB = asFloat(val)
		case "__vmem_gb":
			res.VMemGB = asFloat(val)
		case "__special":
			_ = json.Unmarshal(val, &res.Special) // best-effort; non-string leaves it empty
		default:
			args[key] = val
		}
	}

	return res, args
}

// mergeArgs builds a chunk's _args: the stage args, overlaid with the chunk's
// per-chunk args, plus the resolved resource keys the runtime injects.
func mergeArgs(stageArgs json.RawMessage, chunk ChunkDef, res Resources) (json.RawMessage, error) {
	merged, err := toMap(stageArgs)
	if err != nil {
		return nil, fmt.Errorf("stage args: %w", err)
	}

	maps.Copy(merged, chunk.Args)
	injectResources(merged, resolveResources(chunk.Resources, res))

	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal chunk args: %w", err)
	}

	return raw, nil
}

// withResources returns the stage args with the resolved resource keys added,
// used for the join phase (which has no per-chunk args).
func withResources(stageArgs json.RawMessage, res Resources) (json.RawMessage, error) {
	merged, err := toMap(stageArgs)
	if err != nil {
		return nil, fmt.Errorf("stage args: %w", err)
	}

	injectResources(merged, resolveResources(Resources{}, res))

	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal join args: %w", err)
	}

	return raw, nil
}

// resolveResources overlays a chunk's per-chunk overrides on the phase
// allocation. A non-zero override wins (including negative adaptive sentinels);
// vmem defaults to the resolved memory plus the standard headroom.
func resolveResources(chunk, res Resources) Resources {
	// A negative request is Martian's adaptive sentinel ("at least |x|, ideally
	// all available"); its cluster path resolves it to the positive |x| before
	// reporting it, so do the same here to avoid leaking a negative mem_gb/threads
	// into _jobinfo and the __* keys the stage reads.
	eff := Resources{
		MemGB:   absResource(coalesce(chunk.MemGB, res.MemGB)),
		Threads: absResource(coalesce(chunk.Threads, res.Threads)),
		Special: chunk.Special,
	}

	if eff.Special == "" {
		eff.Special = res.Special
	}

	// vmem defaults to the resolved memory plus the standard headroom. Matching
	// Martian's remote GetSystemReqs, any resolved vmem below 1 GB (an unset 0, or
	// a too-small explicit value) is recomputed from memory rather than used
	// as-is.
	eff.VMemGB = absResource(coalesce(chunk.VMemGB, res.VMemGB))
	if eff.VMemGB < 1 {
		eff.VMemGB = eff.MemGB + extraVMemGB
	}

	return eff
}

// absResource resolves a negative adaptive sentinel to its magnitude.
func absResource(f float64) float64 {
	if f < 0 {
		return -f
	}

	return f
}

// injectResources writes the resolved resource keys into an args map.
func injectResources(merged map[string]json.RawMessage, eff Resources) {
	merged["__mem_gb"] = numRaw(eff.MemGB)
	merged["__threads"] = numRaw(eff.Threads)
	merged["__vmem_gb"] = numRaw(eff.VMemGB)

	if eff.Special != "" {
		if raw, err := json.Marshal(eff.Special); err == nil {
			merged["__special"] = raw
		}
	}
}

func toMap(raw json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}

	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("parse json object: %w", err)
		}
	}

	return out, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}

	return writeRaw(path, data)
}

func writeRaw(path string, data []byte) error {
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	return nil
}

func readRaw(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}

	return data, nil
}

func orEmptyObj(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}

	return raw
}

func numRaw(f float64) json.RawMessage {
	return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64))
}

func asFloat(raw json.RawMessage) float64 {
	var f float64
	// A non-numeric resource value leaves f at 0 (treated as "unset"); Martian
	// always writes numbers here, so a parse failure is not actionable.
	_ = json.Unmarshal(raw, &f)

	return f
}

// coalesce returns primary unless it is unset (0); Martian treats only 0 as
// "no override" and uses negative values as adaptive sentinels.
func coalesce(primary, fallback float64) float64 {
	if primary != 0 {
		return primary
	}

	return fallback
}
