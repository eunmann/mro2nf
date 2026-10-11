package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

// errJoinUnknownKey reports a non-resource key in a split's join override, which
// mrp rejects as an invalid join definition rather than silently ignoring.
var errJoinUnknownKey = errors.New("unknown key in join override")

// errBadResource reports a resource key in a split's chunk def or join override
// whose value has the wrong JSON type (a non-number threads/mem/vmem, or a
// non-string __special), which mrp's updateFromLazyArgs rejects as an unmarshal
// error that fails the split rather than coercing to a default.
var errBadResource = errors.New("invalid resource value")

// prepDirs creates the metadata, files, and per-stage tmp directories for a
// phase and returns the absolute metadata dir, files dir, and journal prefix
// path the adapter writes to. The tmp dir backs the TMPDIR env var the adapter
// child runs with (mrp sets TMPDIR=<meta>/tmp per job — core/node.go,
// metadata.go TempDir — so stage tempfiles land on the job's scratch volume,
// not the possibly tiny container /tmp).
func prepDirs(workDir, phase string) (string, string, string, error) {
	meta := filepath.Join(workDir, phase)
	files := filepath.Join(meta, "files")

	if err := os.MkdirAll(files, dirPerm); err != nil {
		return "", "", "", fmt.Errorf("create work dirs: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(meta, "tmp"), dirPerm); err != nil {
		return "", "", "", fmt.Errorf("create tmp dir: %w", err)
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

// stageNameFile names the stage a phase ran, in a file of mre's own beside the
// adapter metadata. _jobinfo's "name" carries only the top-level call, and the
// Nextflow task graph is not a stable identity, so the mrp ABI differential
// reads this to group invocations by stage. The adapter never reads it, so it
// adds nothing to the _args/_jobinfo/_outs ABI.
const stageNameFile = "_mre_stage"

func writeStageName(meta, stage string) error {
	return writeRaw(filepath.Join(meta, stageNameFile), []byte(stage))
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

	for i, c := range sd.Chunks {
		res, args, err := parseResources(c)
		if err != nil {
			return nil, Resources{}, fmt.Errorf("parse _stage_defs: chunk %d: %w", i, err)
		}

		defs = append(defs, ChunkDef{Args: args, Resources: res})
	}

	join, extra, err := parseResources(sd.Join)
	if err != nil {
		return nil, Resources{}, fmt.Errorf("parse _stage_defs: join override: %w", err)
	}

	// The join override carries only resource keys: mrp fails the split on any
	// other key ("Invalid parameter in join definition").
	if len(extra) > 0 {
		return nil, Resources{}, fmt.Errorf("%w: %s", errJoinUnknownKey, strings.Join(slices.Sorted(maps.Keys(extra)), ", "))
	}

	return defs, join, nil
}

// parseResources splits a Martian arg map into its __-prefixed resource keys and
// the remaining data args, for both a chunk def and the join override. Like
// mrp's updateFromLazyArgs, a value json.Unmarshal cannot decode into the
// resource's type fails (null decodes to unset), and the keys are checked in its
// order so the error names the key mrp's does.
func parseResources(m map[string]json.RawMessage) (Resources, map[string]json.RawMessage, error) {
	var res Resources

	fields := []struct {
		key string
		dst any
	}{
		{"__threads", &res.Threads},
		{"__mem_gb", &res.MemGB},
		{"__vmem_gb", &res.VMemGB},
		{"__special", &res.Special},
	}

	args := make(map[string]json.RawMessage, len(m))
	maps.Copy(args, m)

	for _, f := range fields {
		val, ok := args[f.key]
		if !ok {
			continue
		}

		if err := json.Unmarshal(val, f.dst); err != nil {
			return Resources{}, nil, fmt.Errorf("%w: %s = %s: %w", errBadResource, f.key, val, err)
		}

		delete(args, f.key)
	}

	return res, args, nil
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
// used for the join phase (which has no per-chunk args): join is the split's
// join override, res the phase request.
func withResources(stageArgs json.RawMessage, join, res Resources) (json.RawMessage, error) {
	merged, err := toMap(stageArgs)
	if err != nil {
		return nil, fmt.Errorf("stage args: %w", err)
	}

	injectResources(merged, resolveResources(join, res))

	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal join args: %w", err)
	}

	return raw, nil
}

// resolveResources overlays a chunk's or join's own request (def) on the phase
// request — a non-zero field wins, as in mrp's getJobReqs — and resolves the
// result as mrp's local job manager does (GetSystemReqs): unset threads and
// memory take the 1-thread / 1 GB job defaults, threads round up to whole
// centicores (not CPUs), memory and vmem round up to whole MiB, and vmem
// defaults to memory plus the standard headroom and never falls below memory.
func resolveResources(def, res Resources) Resources {
	// A negative request is Martian's adaptive sentinel ("at least |x|, ideally
	// all available"). mrp's local job manager hands it the host's free capacity,
	// which has no Nextflow-task equivalent, so it resolves to |x| as on mrp's
	// cluster path rather than leaking a negative into _jobinfo and the __* keys.
	eff := Resources{Special: def.Special}

	centi := math.Ceil(math.Abs(coalesce(def.Threads, res.Threads)) * centiPerCore)
	if centi == 0 {
		centi = threadsPerJob * centiPerCore
	}

	eff.Threads = centi / centiPerCore

	if eff.Special == "" {
		eff.Special = res.Special
	}

	memMB := ceilMB(coalesce(def.MemGB, res.MemGB))
	if memMB == 0 {
		memMB = memGBPerJob * mbPerGB
	}

	vmemMB := ceilMB(coalesce(def.VMemGB, res.VMemGB))
	if vmemMB == 0 {
		vmemMB = memMB + extraVMemGB*mbPerGB
	}

	eff.MemGB = memMB / mbPerGB
	eff.VMemGB = max(vmemMB, memMB) / mbPerGB

	return eff
}

// ApplyOverride returns def — the stage's using() request, or a chunk's or
// join's own request — with an operator --overrides value's non-zero fields
// over it. mrp's getJobReqs applies overrides after the chunk or join request,
// so an override wins over both (and is what the stage then sees).
func ApplyOverride(def, override Resources) Resources {
	def.Threads = coalesce(override.Threads, def.Threads)
	def.MemGB = coalesce(override.MemGB, def.MemGB)
	def.VMemGB = coalesce(override.VMemGB, def.VMemGB)

	return def
}

// ceilMB is a GB request's magnitude in whole MiB, rounded up.
func ceilMB(gb float64) float64 {
	return math.Ceil(math.Abs(gb) * mbPerGB)
}

// injectResources writes the resolved resource keys into an args map.
func injectResources(merged map[string]json.RawMessage, eff Resources) {
	merged["__mem_gb"] = resourceNum(eff.MemGB)
	merged["__threads"] = resourceNum(eff.Threads)
	merged["__vmem_gb"] = resourceNum(eff.VMemGB)

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

// resourceNum formats a resolved resource as mrp's JobResources.ToLazyMap
// writes it into _args: three significant digits, and no exponent from 999.5 up.
func resourceNum(f float64) json.RawMessage {
	if f < 999.5 && f > -999.5 {
		return json.RawMessage(strconv.FormatFloat(f, 'g', 3, 64))
	}

	return json.RawMessage(strconv.FormatFloat(f, 'f', 0, 64))
}

// coalesce returns primary unless it is unset (0); Martian treats only 0 as
// "no override" and uses negative values as adaptive sentinels.
func coalesce(primary, fallback float64) float64 {
	if primary != 0 {
		return primary
	}

	return fallback
}
