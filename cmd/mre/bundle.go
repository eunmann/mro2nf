package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// errNotObject is returned when a payload that should be a JSON object isn't.
var errNotObject = errors.New("expected a JSON object")

// producer holds the flags a bundle-writing subcommand needs to locate the file
// leaves it must stage: the type manifest, the producing callable, and which of
// its parameter sets applies.
type producer struct {
	types    string
	callable string
	role     string

	// confine is set by the stage phases, whose output bundle carries files
	// the stage chose; it stays nil for bundles of bound or merged values.
	confine *shim.Confinement

	// manifest is loaded lazily and cached: a single mre invocation reads it
	// from coerceInputs/write/reconstruct, and the path never changes after
	// flag parse, so it is loaded from disk at most once.
	manCache  *types.Manifest
	manLoaded bool
}

func addProducer(fs *flag.FlagSet, defaultRole string) *producer {
	p := &producer{}
	fs.StringVar(&p.types, "types", "", "type manifest (types.json) path")
	fs.StringVar(&p.callable, "callable", "", "producing callable name")
	fs.StringVar(&p.role, "role", defaultRole, "param set: in|out|mainout|chunkin")

	return p
}

// manifest loads the type manifest, or an empty one when no path is configured
// (e.g. pipelines with no file outputs need no rewriting).
func (p *producer) manifest() (types.Manifest, error) {
	if p.manLoaded {
		return *p.manCache, nil
	}

	if p.types == "" {
		p.manCache, p.manLoaded = &types.Manifest{}, true

		return *p.manCache, nil
	}

	man, err := types.LoadManifest(p.types)
	if err != nil {
		return types.Manifest{}, fmt.Errorf("load type manifest: %w", err)
	}

	p.manCache, p.manLoaded = &man, true

	return man, nil
}

// write serializes raw as a bundle directory, staging its file leaves.
func (p *producer) write(dir string, raw json.RawMessage) error {
	payload, params, tbl, err := p.decode(raw)
	if err != nil {
		return fmt.Errorf("write bundle %s: %w", dir, err)
	}

	if err := shim.WriteBundle(dir, payload, params, tbl, p.confine); err != nil {
		return fmt.Errorf("write bundle %s: %w", dir, err)
	}

	return nil
}

// writeBound is write for a payload resolved from spec: a callee's args or a
// pipeline's outputs (see writeBoundBundle).
func (p *producer) writeBound(dir string, raw json.RawMessage, spec bind.Spec) error {
	payload, params, tbl, err := p.decode(raw)
	if err != nil {
		return fmt.Errorf("write bound bundle %s: %w", dir, err)
	}

	return writeBoundBundle(dir, payload, spec, params, tbl)
}

// decode returns raw's payload with the producer's param set and type table.
func (p *producer) decode(raw json.RawMessage) (map[string]any, []ir.Param, *types.Table, error) {
	man, err := p.manifest()
	if err != nil {
		return nil, nil, nil, err
	}

	payload, err := rawToMap(raw)
	if err != nil {
		return nil, nil, nil, err
	}

	params, err := man.Params(p.callable, p.role)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("producer params: %w", err)
	}

	return payload, params, man.Table(), nil
}

// writeBoundBundle writes a payload resolved from spec as a bundle after
// narrowing each value to its receiving param's type, as mrp filters every
// bound value (#239, bind.Narrow): a field the receiver does not declare, and
// any file leaf inside it, never reaches the callee. Its files are the run's
// own inputs, so the write is unconfined.
func writeBoundBundle(dir string, payload map[string]any, spec bind.Spec, params []ir.Param, tbl *types.Table) error {
	if err := shim.WriteBundle(dir, bind.Narrow(spec, payload, params, tbl), params, tbl, nil); err != nil {
		return fmt.Errorf("write bound bundle %s: %w", dir, err)
	}

	return nil
}

// warnOutputScratchRefs loudly flags a stage output that baked this task's
// ephemeral work-dir absolute path into its file CONTENT (rather than emitting a
// declared file leaf). Such a path resolves under mrp's shared filesystem but
// not on an object-store backend (AWS Batch + S3, HealthOmics), where the
// downstream task is an isolated container — a silent divergence the data plane
// cannot fix (it stages declared leaves, not paths hidden in bytes). Best-effort
// and non-fatal: the warning goes to stderr (forwarded to the task log →
// CloudWatch / the Omics engine log), turning a cryptic downstream
// FileNotFoundError into a clear, stage-local diagnostic.
func warnOutputScratchRefs(bundleDir, work string) {
	prefix, err := filepath.Abs(work)
	if err != nil {
		return
	}

	for _, ref := range shim.ScanOutputForScratchRefs(bundleDir, prefix) {
		fmt.Fprintf(os.Stderr, "mre: WARNING: stage output %s embeds an absolute "+
			"scratch path %q — this resolves on a shared filesystem (mrp, local) but "+
			"NOT on an object-store backend (AWS Batch + S3, HealthOmics), where the "+
			"downstream task is an isolated container. Emit the referenced file as a "+
			"declared Martian file output instead of baking its path into content.\n",
			ref.File, ref.Path)
	}
}

// coerceInputs coerces whole-number float values bound to the callable's int
// input params back to integers in raw, as mrp's FilterJson does for every
// bound value. A split's chunk args never pass through here: mrp's
// ChunkDef.Merge hands them to main as the split wrote them (#245). Returns raw
// unchanged when no manifest is configured.
func (p *producer) coerceInputs(raw json.RawMessage) (json.RawMessage, error) {
	if p.types == "" || len(raw) == 0 {
		return raw, nil
	}

	man, err := p.manifest()
	if err != nil {
		return nil, err
	}

	vals, err := rawToMap(raw)
	if err != nil {
		return nil, err
	}

	params, err := man.Params(p.callable, types.RoleIn)
	if err != nil {
		return nil, fmt.Errorf("coerce inputs: %w", err)
	}

	out, err := json.Marshal(man.Table().CoerceScalars(params, vals))
	if err != nil {
		return nil, fmt.Errorf("encode coerced args: %w", err)
	}

	return out, nil
}

// validateOuts applies mrp's stage output validation to a stage's final
// outputs (types.ValidateOutputs): mrp fails the stage on an invalid _outs, so
// the task fails too rather than hand the value downstream. Per-chunk outputs
// are not passed here, as mrp's default enforcement level skips them, and a
// callable with no declared outputs is not checked.
func (p *producer) validateOuts(raw json.RawMessage) error {
	man, err := p.manifest()
	if err != nil {
		return err
	}

	params, err := man.Params(p.callable, types.RoleOut)
	if err != nil {
		return fmt.Errorf("validate outputs: %w", err)
	}

	if len(params) == 0 {
		return nil
	}

	outs, err := rawToMap(raw)
	if err != nil {
		return fmt.Errorf("validate outputs: %w", err)
	}

	if err := man.Table().ValidateOutputs(params, outs); err != nil {
		return fmt.Errorf("validate outputs of %s: %w", p.callable, err)
	}

	return nil
}

// rawToMap decodes a JSON object into a map of its values, each decoded by
// types.DecodeJSON, so a number keeps its token (42.0 stays 42.0) and a nested
// object its key order across the bundle round-trip; the top level is written
// back with its keys sorted, as mrp writes every _args and _outs. An empty
// payload, JSON null, or the empty string the Martian adapter writes for a
// stage with no outputs yields an empty map. Any other non-object payload (an
// array, a number, or corrupt JSON) is an error, so genuinely malformed outputs
// surface instead of silently becoming empty.
func rawToMap(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		return out, nil
	}

	if trimmed[0] != '{' {
		return nil, fmt.Errorf("decode payload: %w, got %.32s", errNotObject, trimmed)
	}

	v, err := types.DecodeJSON(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	obj, _ := types.AsObject(v)
	for _, m := range obj {
		out[m.Key] = m.Value
	}

	return out, nil
}
