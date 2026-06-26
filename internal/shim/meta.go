package shim

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
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
			"martian":   "martian-nextflow",
			"pipelines": "martian-nextflow",
		},
	}

	return writeJSON(filepath.Join(meta, "_jobinfo"), info)
}

// writeSkeletonOuts writes an _outs file with every output name set to null,
// which the adapter reads and then populates.
func writeSkeletonOuts(meta string, names []string) error {
	outs := make(map[string]any, len(names))
	for _, n := range names {
		outs[n] = nil
	}

	return writeJSON(filepath.Join(meta, "_outs"), outs)
}

// readStageDefs parses a split phase's _stage_defs into chunk definitions,
// separating the __-prefixed resource keys from the data args.
func readStageDefs(meta string) ([]ChunkDef, error) {
	raw, err := readRaw(filepath.Join(meta, "_stage_defs"))
	if err != nil {
		return nil, err
	}

	var sd struct {
		Chunks []map[string]json.RawMessage `json:"chunks"`
	}

	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, fmt.Errorf("parse _stage_defs: %w", err)
	}

	defs := make([]ChunkDef, 0, len(sd.Chunks))
	for _, c := range sd.Chunks {
		defs = append(defs, splitChunk(c))
	}

	return defs, nil
}

func splitChunk(chunk map[string]json.RawMessage) ChunkDef {
	def := ChunkDef{Args: make(map[string]json.RawMessage, len(chunk))}

	for key, val := range chunk {
		switch key {
		case "__threads":
			def.Resources.Threads = asFloat(val)
		case "__mem_gb":
			def.Resources.MemGB = asFloat(val)
		case "__vmem_gb":
			def.Resources.VMemGB = asFloat(val)
		case "__special":
			_ = json.Unmarshal(val, &def.Resources.Special) // best-effort; non-string leaves it empty
		default:
			def.Args[key] = val
		}
	}

	return def
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
	eff := Resources{
		MemGB:   coalesce(chunk.MemGB, res.MemGB),
		Threads: coalesce(chunk.Threads, res.Threads),
		Special: chunk.Special,
	}

	if eff.Special == "" {
		eff.Special = res.Special
	}

	eff.VMemGB = coalesce(chunk.VMemGB, coalesce(res.VMemGB, eff.MemGB+extraVMemGB))

	return eff
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
