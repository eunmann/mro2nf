// Package emit renders a transpiler IR program into a runnable Nextflow
// project: main.nf, nextflow.config, per-call binding specs, and the entry
// args. Each generated process invokes the mre shim against the original
// Martian stage code.
package emit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/apperror"
	"github.com/eunmann/martian-nextflow/internal/bind"
	"github.com/eunmann/martian-nextflow/internal/ir"
	"github.com/eunmann/martian-nextflow/internal/shim"
	"github.com/eunmann/martian-nextflow/internal/types"
)

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// errNoEntry indicates the program has no top-level call to drive.
var errNoEntry = errors.New("program has no entry call")

// Options configures emission. All executable paths should be absolute so the
// generated project can run from any working directory.
type Options struct {
	// OutDir is the directory to write the Nextflow project into.
	OutDir string
	// Mre is the path to the mre shim binary.
	Mre string
	// Shell is the path to martian_shell.py.
	Shell string
	// Mrjob is the path to the mrjob wrapper (for comp stages); may be empty.
	Mrjob string
	// MROFile is the source MRO filename recorded in _jobinfo.
	MROFile string
	// StageCode maps each stage name to its (absolute) stage code path.
	StageCode map[string]string
	// Container, when set, is the image used for every process (process.container
	// in nextflow.config) — required by container backends like AWS Batch.
	Container string
}

// Emit writes the Nextflow project for prog into opts.OutDir.
func Emit(prog *ir.Program, opts Options) error {
	if prog.Entry == nil {
		return errNoEntry
	}

	if err := validateProgram(prog); err != nil {
		return err
	}

	specDir := filepath.Join(opts.OutDir, "bindspecs")
	modDir := filepath.Join(opts.OutDir, "modules")

	for _, dir := range []string{specDir, modDir} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create output dirs: %w", err)
		}
	}

	g := genCtx{
		entry:   prog.Entry.Callable,
		mroFile: opts.MROFile,
		mre:     opts.Mre,
		shell:   opts.Shell,
		mrjob:   opts.Mrjob,
		code:    opts.StageCode,
	}

	if err := writeModules(prog, modDir, g); err != nil {
		return err
	}

	if err := writeFile(filepath.Join(opts.OutDir, "main.nf"), []byte(generateMain(prog, g))); err != nil {
		return err
	}

	if err := writeFile(filepath.Join(opts.OutDir, "nextflow.config"), []byte(configFile(opts.Container))); err != nil {
		return err
	}

	if err := writeBindSpecs(prog, specDir); err != nil {
		return err
	}

	if err := writeDisableArtifacts(prog, opts.OutDir, specDir); err != nil {
		return err
	}

	if err := types.BuildManifest(prog).Write(filepath.Join(opts.OutDir, "types.json")); err != nil {
		return fmt.Errorf("write types manifest: %w", err)
	}

	return writeEntryArgs(prog, opts.OutDir)
}

// writeDisableArtifacts emits, for every disabled call, its disable bindspec
// and a null-outputs file used when the call is skipped at runtime.
func writeDisableArtifacts(prog *ir.Program, outDir, specDir string) error {
	nullsDir := filepath.Join(outDir, "nulls")

	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		for _, c := range p.Calls {
			if c.Disabled == nil {
				continue
			}

			if err := os.MkdirAll(nullsDir, dirPerm); err != nil {
				return fmt.Errorf("create nulls dir: %w", err)
			}

			spec := bindSpec(prog, p, disableBindings(c))
			if err := writeJSONFile(filepath.Join(specDir, disableName(p.Name, c.Name)+".json"), spec); err != nil {
				return err
			}

			// A skipped call emits a null-valued output bundle (no files).
			nulls := nullOuts(prog, c.Callable)
			nullDir := filepath.Join(nullsDir, qualify(p.Name, c.Name))
			if err := shim.WriteBundle(nullDir, nulls, nil, types.NewTable(prog.Structs)); err != nil {
				return fmt.Errorf("write null bundle: %w", err)
			}
		}
	}

	return nil
}

// nullOuts returns a map of a callable's output names to null.
func nullOuts(prog *ir.Program, callable string) map[string]any {
	out := map[string]any{}

	if s, ok := prog.Stages[callable]; ok {
		for _, p := range s.Out {
			out[p.Name] = nil
		}
	}

	if p, ok := prog.Pipelines[callable]; ok {
		for _, param := range p.Out {
			out[param.Name] = nil
		}
	}

	return out
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}

	return writeFile(path, data)
}

// validateProgram rejects feature combinations the emitter cannot yet lower
// correctly, with a clear error rather than silently wrong output.
func validateProgram(prog *ir.Program) error {
	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		for _, c := range p.Calls {
			if err := validateMapCall(prog, name, c); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateMapCall rejects map-call modifier combinations the keyed machinery
// cannot yet thread per fork.
func validateMapCall(prog *ir.Program, pipeline string, c ir.Call) error {
	if !c.Mapped {
		return nil
	}

	// Stage and keyable-pipeline map targets run through fork-key-threaded
	// variants. A pipeline target whose body (transitively) contains a disabled
	// or nested-map call cannot yet be keyed per fork.
	if !keyablePipeline(prog, c.Callable, map[string]bool{}) {
		return &apperror.UnsupportedError{
			Construct: "map call over a pipeline with disabled or nested-map calls",
			Detail:    pipeline + "." + c.Name + " -> " + c.Callable,
		}
	}

	return nil
}

// keyablePipeline reports whether a map call may target callable: stages are
// always fine; a pipeline is keyable only if every call in it (transitively) is
// a plain or split call — a disabled or nested-map call inside the body cannot
// yet be threaded per fork. Recursion is bounded by the seen set.
func keyablePipeline(prog *ir.Program, callable string, seen map[string]bool) bool {
	if seen[callable] {
		return true
	}

	seen[callable] = true

	p, ok := prog.Pipelines[callable]
	if !ok {
		return true // a stage target
	}

	for _, c := range p.Calls {
		if c.Disabled != nil || c.Mapped {
			return false
		}

		if !keyablePipeline(prog, c.Callable, seen) {
			return false
		}
	}

	return true
}

// mapProjectDepth returns how many leading path segments a ref navigates before
// it must project the remainder over a typed map's values (a map<S>.field
// projection). It returns 0 when there is no typed-map projection — arrays
// auto-project at runtime and structs navigate by key. The binder uses the depth
// to switch from key navigation to map projection at exactly the right segment.
func mapProjectDepth(prog *ir.Program, p *ir.Pipeline, ref *ir.Ref) int {
	if p == nil || ref == nil || ref.Output == "" {
		return 0
	}

	var segs []string

	var cur *ir.Param

	// Track the array and typed-map dims of the value reached so far. A map call
	// wraps the callee's outputs in one extra dimension (map-mode -> map; array-
	// mode -> array). We project a field through a value only when it is a typed
	// map with no enclosing array (an array auto-projects at runtime; a field
	// beneath an array-of-map is the rare unhandled shape, left to runtime).
	curMap, curArray := 0, 0

	switch ref.Kind {
	case "self":
		segs = append([]string{ref.ID}, strings.Split(ref.Output, ".")...)
		cur = paramByName(p.In, segs[0])
	case "call":
		segs = strings.Split(ref.Output, ".")
		cur = paramByName(calleeOutParams(prog, p, ref.ID), segs[0])
		curMap, curArray = forkDims(findCall(p, ref.ID))
	default:
		return 0
	}

	if cur != nil {
		curMap += cur.MapDim
		curArray += cur.ArrayDim
	}

	for i := 1; i < len(segs); i++ {
		if cur == nil || curArray > 0 {
			return 0
		}

		if curMap > 0 { // a field access beneath a (non-array) typed map
			return i
		}

		st, ok := prog.Structs[cur.BaseType]
		if !ok {
			return 0
		}

		cur = paramByName(st.Fields, segs[i])
		if cur != nil {
			curMap, curArray = cur.MapDim, cur.ArrayDim
		}
	}

	return 0
}

// forkDims returns the extra map and array dimensions a map call wraps its
// callee's outputs in: a keyed (map/unknown-keyed) fork adds a map dimension; an
// array fork adds an array dimension.
func forkDims(c *ir.Call) (int, int) {
	if c == nil {
		return 0, 0
	}

	switch c.MapMode {
	case "map", "unknown":
		return 1, 0
	case "array":
		return 0, 1
	default:
		return 0, 0
	}
}

// findCall returns the call with the given instance id in p, or nil.
func findCall(p *ir.Pipeline, id string) *ir.Call {
	for i := range p.Calls {
		if p.Calls[i].Name == id {
			return &p.Calls[i]
		}
	}

	return nil
}

func paramByName(ps []ir.Param, name string) *ir.Param {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}

	return nil
}

// calleeOutParams returns the output params of the callable invoked by the named
// call in pipeline p.
func calleeOutParams(prog *ir.Program, p *ir.Pipeline, callID string) []ir.Param {
	for _, c := range p.Calls {
		if c.Name != callID {
			continue
		}

		if s, ok := prog.Stages[c.Callable]; ok {
			return s.Out
		}

		if pp, ok := prog.Pipelines[c.Callable]; ok {
			return pp.Out
		}
	}

	return nil
}

// writeModules writes one Nextflow module per stage and per pipeline.
func writeModules(prog *ir.Program, modDir string, g genCtx) error {
	for _, name := range sortedKeys(prog.Stages) {
		path := filepath.Join(modDir, "stage_"+name+".nf")
		if err := writeFile(path, []byte(generateStageModule(prog.Stages[name], g))); err != nil {
			return err
		}
	}

	for _, name := range sortedKeys(prog.Pipelines) {
		path := filepath.Join(modDir, "pipe_"+name+".nf")
		if err := writeFile(path, []byte(generatePipeModule(prog.Pipelines[name], prog, g))); err != nil {
			return err
		}
	}

	return nil
}

func writeBindSpecs(prog *ir.Program, specDir string) error {
	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		for _, c := range p.Calls {
			if err := writeSpec(specDir, bindName(p.Name, c.Name), prog, p, c.Bindings); err != nil {
				return err
			}
		}

		if err := writeSpec(specDir, bindName(p.Name, "return"), prog, p, p.Returns); err != nil {
			return err
		}
	}

	return nil
}

func writeSpec(specDir, name string, prog *ir.Program, p *ir.Pipeline, bindings []ir.Binding) error {
	data, err := json.MarshalIndent(bindSpec(prog, p, bindings), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spec %s: %w", name, err)
	}

	return writeFile(filepath.Join(specDir, name+".json"), data)
}

// writeEntryArgs resolves the top-level call's inputs and writes them as the
// entry_args bundle, staging any file-typed entry inputs so the run is
// self-contained from the start.
func writeEntryArgs(prog *ir.Program, outDir string) error {
	args, err := bind.Resolve(bindSpec(prog, nil, prog.Entry.Bindings), nil, nil)
	if err != nil {
		return fmt.Errorf("resolve entry args: %w", err)
	}

	payload := map[string]any{}
	if len(args) > 0 {
		// UseNumber keeps a whole-number float (e.g. 21.0) from collapsing to an
		// integer when the entry args round-trip through the bundle.
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.UseNumber()

		if err := dec.Decode(&payload); err != nil {
			return fmt.Errorf("decode entry args: %w", err)
		}
	}

	if err := shim.WriteBundle(filepath.Join(outDir, "entry_args"), payload, entryInParams(prog), types.NewTable(prog.Structs)); err != nil {
		return fmt.Errorf("write entry args bundle: %w", err)
	}

	return nil
}

// entryInParams returns the entry callable's input parameters.
func entryInParams(prog *ir.Program) []ir.Param {
	if p, ok := prog.Pipelines[prog.Entry.Callable]; ok {
		return p.In
	}

	if s, ok := prog.Stages[prog.Entry.Callable]; ok {
		return s.In
	}

	return nil
}

// bindSpec converts IR bindings into a runtime binding spec, skipping wildcard
// bindings (handled separately). prog and the enclosing pipeline p (may be nil
// for the top-level entry) let it resolve typed-map field projections.
func bindSpec(prog *ir.Program, p *ir.Pipeline, bindings []ir.Binding) bind.Spec {
	spec := bind.Spec{}

	for _, b := range bindings {
		if b.Param == "*" {
			continue
		}

		entry := valueToEntry(prog, p, b.Value)
		entry.Split = b.Split
		spec[b.Param] = entry
	}

	return spec
}

// configFile renders nextflow.config with executor profiles. The local and
// HPC profiles (slurm/sge/lsf/pbs) work with the shared-filesystem model used
// today; cloud profiles additionally require the object-store data plane.
// valueToEntry converts an IR value tree into a runtime bind.Entry, preserving
// refs nested inside array/object literals.
func valueToEntry(prog *ir.Program, p *ir.Pipeline, v ir.Value) bind.Entry {
	switch {
	case v.Array != nil:
		arr := make([]bind.Entry, len(v.Array))
		for i, e := range v.Array {
			arr[i] = valueToEntry(prog, p, e)
		}

		return bind.Entry{Array: arr}
	case v.Object != nil:
		obj := make(map[string]bind.Entry, len(v.Object))
		for k, e := range v.Object {
			obj[k] = valueToEntry(prog, p, e)
		}

		return bind.Entry{Object: obj}
	case v.Ref != nil:
		return bind.Entry{Ref: &bind.Ref{
			Kind:     v.Ref.Kind,
			ID:       v.Ref.ID,
			Output:   v.Ref.Output,
			MapDepth: mapProjectDepth(prog, p, v.Ref),
		}}
	default:
		return bind.Entry{Literal: v.Literal}
	}
}

func configFile(container string) string {
	containerLine := ""
	if container != "" {
		containerLine = "    container = '" + container + "'\n"
	}

	return `params.outdir = 'results'
// Cloud knobs the awsbatch profile reads; override with --aws_queue/--aws_region.
params.aws_queue = null
params.aws_region = null

// Coarse analog of mrp --autoretry: retry a failed task a couple of times.
// (mrp's content-based ASSERT-vs-retryable classification has no Nextflow
// equivalent, so this retries any failure.) Cap concurrency with the standard
// '-process.maxForks' / '-qs' flags; local pools with '-process.cpus' etc.
process {
    errorStrategy = 'retry'
    maxRetries = 2
` + containerLine + `}

profiles {
    standard { process.executor = 'local' }
    slurm    { process.executor = 'slurm' }
    sge      { process.executor = 'sge' }
    lsf      { process.executor = 'lsf' }
    pbs      { process.executor = 'pbs' }
    awsbatch {
        process.executor = 'awsbatch'
        process.queue    = params.aws_queue
        aws.region       = params.aws_region
    }
    k8s { process.executor = 'k8s' }
}
`
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	return nil
}
