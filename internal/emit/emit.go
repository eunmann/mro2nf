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
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

const (
	dirPerm  = 0o755
	filePerm = 0o644
	// assetsDir holds types.json + bindspecs/. Each task stages the individual
	// files it needs (the shared types.json, plus its own bindspec for a bind/fork
	// process) as `path` inputs, so they are present in an isolated task container
	// (AWS Batch + S3, HealthOmics) and not just on a shared filesystem — and a
	// task transfers only its own bindspec, not every call's.
	assetsDir = "_assets"
	// entrySentinel is a baked empty file fed to BUILD_ENTRY_ARGS for a file-typed
	// entry input the run did not override, so the process still has a staged path
	// input (and keeps the baked default). Lives under _assets/ so it is packaged
	// for HealthOmics. See genEntry.
	entrySentinel = ".entry_empty"
)

// hasFileLeaf reports whether a param has any file leaf — directly (a
// file/dir/path at any array/map dimension) or nested inside a struct field
// (recursively). Such an entry input is staged through Nextflow so its file
// leaves are localized into the (possibly isolated) task; see genEntry.
func hasFileLeaf(p ir.Param, structs map[string]*ir.StructType) bool {
	if p.IsFile {
		return true
	}

	st, ok := structs[p.BaseType]
	if !ok {
		return false
	}

	return slices.ContainsFunc(st.Fields, func(f ir.Param) bool {
		return hasFileLeaf(f, structs)
	})
}

// hasStagedFileEntry reports whether the entry callable has any file-bearing
// input staged as a run parameter (so the sentinel file is worth baking).
func hasStagedFileEntry(prog *ir.Program) bool {
	return slices.ContainsFunc(entryInParams(prog), func(p ir.Param) bool {
		return hasFileLeaf(p, prog.Structs)
	})
}

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
	// MRODir is the source .mro's directory. A relative file path baked into an
	// entry input default is resolved against it so the baked entry_args bundle is
	// self-contained regardless of the transpile working directory.
	MRODir string
	// StageCode maps each stage name to its (absolute) stage code path.
	StageCode map[string]string
	// Container, when set, is the image used for every process (process.container
	// in nextflow.config) — required by container backends like AWS Batch.
	Container string
	// Monitor enables per-stage virtual-memory enforcement in the shim (the mrp
	// --monitor analog).
	Monitor bool
	// FoldDisables opts into constant-folding entry-determinable disable branches
	// (#59 Lever 1): an always-disabled stage is pruned from the emitted project.
	FoldDisables bool
	// FuseChains opts into linear-chain stage fusion (#59 Lever 4): a single-
	// consumer, equal-resource source stage is folded into its consumer's task,
	// dropping a node at the cost of coarser -resume/retry granularity.
	FuseChains bool
	// Target is the execution backend the project is shaped for (default local).
	Target Target
}

// Emit writes the Nextflow project for prog into opts.OutDir.
func Emit(prog *ir.Program, opts Options) error {
	if prog.Entry == nil {
		return errNoEntry
	}

	if err := checkSupported(prog); err != nil {
		return err
	}

	if err := checkCompMrjob(prog, opts.Mrjob); err != nil {
		return err
	}

	// types.json and the bindspecs live under _assets/. A process script can only
	// read files staged into its (isolated) task dir — referencing them via
	// ${projectDir} works on a shared filesystem but not on AWS Batch + S3, where
	// the worker is a separate container. Each task stages the specific asset files
	// it needs (types.json, and its own bindspec for binds) so both cases work.
	specDir := filepath.Join(opts.OutDir, assetsDir, "bindspecs")
	modDir := filepath.Join(opts.OutDir, "modules")

	for _, dir := range []string{specDir, modDir} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create output dirs: %w", err)
		}
	}

	target, err := ParseTarget(string(opts.Target))
	if err != nil {
		return err
	}

	features := featureSet{fuseChains: opts.FuseChains, foldDisables: opts.FoldDisables}
	g := genCtx{
		entry:    prog.Entry.Callable,
		mroFile:  opts.MROFile,
		mre:      opts.Mre,
		shell:    opts.Shell,
		mrjob:    opts.Mrjob,
		monitor:  opts.Monitor,
		features: features,
		code:     opts.StageCode,
		plan:     buildPlan(prog, features),
	}

	// Container targets bake in-container paths and ship a self-contained Docker
	// build context (mre + adapters + stage code), so the image — not the host —
	// supplies the runtime.
	if target.isContainer() {
		cb, err := containerBuild(opts, target)
		if err != nil {
			return err
		}

		g.mre, g.shell, g.mrjob, g.code = cb.mre, cb.shell, cb.mrjob, cb.code
	}

	return writeProject(prog, opts, target, g, specDir, modDir)
}

// writeProject renders every file of the Nextflow project (modules, main.nf,
// config, bindspecs, type manifest, disable artifacts, target packaging, and the
// entry args) into opts.OutDir.
func writeProject(prog *ir.Program, opts Options, target Target, g genCtx, specDir, modDir string) error {
	if err := writeModules(prog, modDir, g); err != nil {
		return err
	}

	if err := writeFile(filepath.Join(opts.OutDir, "main.nf"), []byte(generateMain(prog, g))); err != nil {
		return err
	}

	if err := writeFile(filepath.Join(opts.OutDir, "nextflow.config"), []byte(configFile(opts.Container, target))); err != nil {
		return err
	}

	if err := writeLib(opts.OutDir); err != nil {
		return err
	}

	if err := writeBindSpecs(prog, specDir); err != nil {
		return err
	}

	if err := writeDisableArtifacts(prog, opts.OutDir, specDir); err != nil {
		return err
	}

	if err := types.BuildManifest(prog).Write(filepath.Join(opts.OutDir, assetsDir, "types.json")); err != nil {
		return fmt.Errorf("write types manifest: %w", err)
	}

	if target == TargetHealthOmics {
		if err := writeHealthOmicsPackaging(prog, opts.OutDir); err != nil {
			return err
		}
	}

	if hasStagedFileEntry(prog) {
		// A staged-but-unset file input is fed this empty sentinel so BUILD_ENTRY_ARGS
		// still has its path input (and keeps the baked default); see genEntry.
		if err := writeFile(filepath.Join(opts.OutDir, assetsDir, entrySentinel), []byte{}); err != nil {
			return err
		}
	}

	return writeEntryArgs(prog, opts.OutDir, opts.MRODir)
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

// checkCompMrjob rejects a program that declares a comp-adapter stage when no
// -mrjob path was supplied. The generated stage command would omit -mrjob and
// every phase of that stage would fail inside the shim at run time; failing the
// transpile keeps the contract that unsupportable programs never emit a project
// (see Warnings).
func checkCompMrjob(prog *ir.Program, mrjob string) error {
	if mrjob != "" {
		return nil
	}

	for _, name := range sortedKeys(prog.Stages) {
		if prog.Stages[name].Lang == ir.LangComp {
			return &apperror.UnsupportedError{
				Construct: "comp stage " + name,
				Detail:    "-mrjob is required to run comp-adapter stages",
			}
		}
	}

	return nil
}

// checkSupported rejects programs that use a Martian construct with no faithful
// Nextflow lowering before any output is written. It reuses mapProjectDepth's
// shape analysis: a negative depth marks a nested typed-map field projection
// (map<map<S>>.field, or maps nested inside an array), which the binder would
// silently mis-navigate. The array<map<S>>.field shape IS supported (MapInArray).
func checkSupported(prog *ir.Program) error {
	for _, name := range sortedKeys(prog.Pipelines) {
		p := prog.Pipelines[name]

		bindings := append([]ir.Binding{}, p.Returns...)
		for _, c := range p.Calls {
			bindings = append(bindings, c.Bindings...)

			// A `disabled = REF.field` condition navigates the same projection
			// shapes as a binding, so it must pass the same support check.
			if c.Disabled != nil {
				if err := checkValueRefs(prog, p, ir.Value{Ref: c.Disabled}); err != nil {
					return err
				}
			}
		}

		for _, b := range bindings {
			if err := checkValueRefs(prog, p, b.Value); err != nil {
				return err
			}
		}
	}

	return nil
}

// checkValueRefs walks a binding value tree and rejects any ref whose projection
// shape is unsupported (mapProjectDepth < 0).
func checkValueRefs(prog *ir.Program, p *ir.Pipeline, v ir.Value) error {
	switch {
	case v.Array != nil:
		for _, e := range v.Array {
			if err := checkValueRefs(prog, p, e); err != nil {
				return err
			}
		}
	case v.Object != nil:
		for _, e := range v.Object {
			if err := checkValueRefs(prog, p, e); err != nil {
				return err
			}
		}
	case v.Ref != nil:
		if d, _ := mapProjectDepth(prog, p, v.Ref); d < 0 {
			return &apperror.UnsupportedError{
				Construct: "nested typed-map field projection",
				Detail:    fmt.Sprintf("%s.%s in pipeline %s", v.Ref.ID, v.Ref.Output, p.Name),
			}
		}
	}

	return nil
}

// mapProjectDepth returns how many leading path segments a ref navigates before
// it must project the remainder over a typed map's values (a map<S>.field
// projection). It returns 0 when there is no typed-map projection — arrays
// auto-project at runtime and structs navigate by key. The binder uses the depth
// to switch from key navigation to map projection at exactly the right segment.
func mapProjectDepth(prog *ir.Program, p *ir.Pipeline, ref *ir.Ref) (int, bool) {
	if p == nil || ref == nil || ref.Output == "" {
		return 0, false
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
	case refKindCall:
		segs = strings.Split(ref.Output, ".")
		cur = paramByName(calleeOutParams(prog, p, ref.ID), segs[0])
		curMap, curArray = forkDims(findCall(p, ref.ID))
	default:
		return 0, false
	}

	if cur != nil {
		curMap += cur.MapDim
		curArray += cur.ArrayDim
	}

	for i := 1; i < len(segs); i++ {
		if cur == nil {
			return 0, false
		}

		if depth, inArray, done := projectionShape(curArray, curMap, i); done {
			return depth, inArray
		}

		st, ok := prog.Structs[cur.BaseType]
		if !ok {
			return 0, false
		}

		cur = paramByName(st.Fields, segs[i])
		if cur != nil {
			curMap, curArray = cur.MapDim, cur.ArrayDim
		}
	}

	return 0, false
}

// projectionShape decides the projection at a field-access segment from the
// array/map dims of the value reached. It returns (depth, inArray, done): when
// done is true the caller returns (depth, inArray) — depth i for a typed-map
// projection (inArray for array<map<S>>.field), 0 for a plain array auto-project,
// or -1 to reject a nested typed-map projection that has no faithful lowering.
// When done is false the value is a struct and the caller keeps descending.
func projectionShape(curArray, curMap, i int) (int, bool, bool) {
	switch {
	case curArray > 0 && curMap == 1:
		return i, true, true // array<map<S>>.field -> project over each map in the array
	case curArray > 0 && curMap > 1:
		return -1, false, true // nested maps inside an array: unsupported
	case curArray > 0:
		return 0, false, true // a field beneath a plain array auto-projects
	case curMap == 1:
		return i, false, true // map<S>.field
	case curMap > 1:
		return -1, false, true // nested typed-map projection: unsupported
	}

	return 0, false, false // a struct: keep descending by key
}

// forkDims returns the extra map and array dimensions a map call wraps its
// callee's outputs in: a keyed (map/unknown-keyed) fork adds a map dimension; an
// array fork adds an array dimension.
func forkDims(c *ir.Call) (int, int) {
	if c == nil {
		return 0, 0
	}

	switch c.MapMode {
	case mapModeMap, mapModeUnknown:
		return 1, 0
	case mapModeArray:
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
		// A stage fused into every one of its call sites has no importer — its
		// module is dead, so skip it (#82); the fused processes are self-contained.
		if !g.plan.modules[name] {
			continue
		}

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
// self-contained from the start. Relative file-default paths are resolved
// against mroDir so the bundle does not depend on the transpile working dir.
func writeEntryArgs(prog *ir.Program, outDir, mroDir string) error {
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

	params := entryInParams(prog)
	tbl := types.NewTable(prog.Structs)

	if mroDir != "" {
		if payload, err = resolveEntryFileLeaves(payload, params, tbl, mroDir); err != nil {
			return err
		}
	}

	if err := shim.WriteBundle(filepath.Join(outDir, "entry_args"), payload, params, tbl); err != nil {
		return fmt.Errorf("write entry args bundle: %w", err)
	}

	return nil
}

// resolveEntryFileLeaves rewrites every relative file-leaf path in the entry
// payload to an absolute path under mroDir, so a baked default like
// "input/reads.txt" resolves regardless of the transpile working directory. URIs
// (e.g. s3://) and absolute paths pass through unchanged.
func resolveEntryFileLeaves(payload map[string]any, params []ir.Param, tbl *types.Table, mroDir string) (map[string]any, error) {
	resolved, err := tbl.Apply(params, payload, func(path string) (string, error) {
		if path == "" || filepath.IsAbs(path) || strings.Contains(path, "://") {
			return path, nil
		}

		return filepath.Join(mroDir, path), nil
	})
	if err != nil {
		return nil, fmt.Errorf("resolve entry file defaults: %w", err)
	}

	return resolved, nil
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
		// An empty composite must encode as a literal: a non-nil empty Array is
		// dropped by omitempty on marshal, so reload would resolve it to null.
		if len(v.Array) == 0 {
			return bind.Entry{Literal: json.RawMessage("[]")}
		}

		arr := make([]bind.Entry, len(v.Array))
		for i, e := range v.Array {
			arr[i] = valueToEntry(prog, p, e)
		}

		return bind.Entry{Array: arr}
	case v.Object != nil:
		if len(v.Object) == 0 {
			return bind.Entry{Literal: json.RawMessage("{}")}
		}

		obj := make(map[string]bind.Entry, len(v.Object))
		for k, e := range v.Object {
			obj[k] = valueToEntry(prog, p, e)
		}

		return bind.Entry{Object: obj}
	case v.Ref != nil:
		// checkSupported rejects the negative (unsupported) case before emit, so
		// clamp defensively here for any ref reached outside that pass. inArray
		// marks the array<map<S>>.field shape for the binder's projection.
		depth, inArray := mapProjectDepth(prog, p, v.Ref)

		return bind.Entry{Ref: &bind.Ref{
			Kind:       v.Ref.Kind,
			ID:         v.Ref.ID,
			Output:     v.Ref.Output,
			MapDepth:   max(depth, 0),
			MapInArray: inArray,
		}}
	default:
		return bind.Entry{Literal: v.Literal}
	}
}

// configFile renders nextflow.config for the chosen target: the common params +
// retry/process block, the per-target executor wiring, and (cloud targets) a
// parameterized container image.
func configFile(container string, target Target) string {
	var b strings.Builder

	b.WriteString(configCommon(target))
	b.WriteString(configProcess(container, target))

	switch target {
	case TargetAWSBatch:
		b.WriteString(configAWSBatch())
	case TargetHealthOmics:
		b.WriteString(configHealthOmics())
	case TargetLocal:
		b.WriteString(configProfiles())
	}

	return b.String()
}

// configCommon renders the params shared by every target.
func configCommon(target Target) string {
	common := outdirConfig(target) + `
// Martian 'special' scheduler-resource map (the MRO_JOBRESOURCES analog): maps a
// stage's special key to scheduler options applied via clusterOptions on grid
// executors. Empty by default (no-op); override per deployment, e.g.
// --job_resources.highmem='--partition=highmem' or in a -params-file.
params.job_resources = [:]
`
	if target == TargetAWSBatch || target == TargetLocal {
		common += `// Cloud knobs the awsbatch profile reads; override with --aws_queue/--aws_region.
params.aws_queue = null
params.aws_region = null
`
	}

	return common
}

// outdirConfig renders the params.outdir declaration. AWS Batch defaults to no
// curated publish (params.outdir = null): every stage's declared outputs are
// already uploaded to the S3 work dir by the Batch executor, so the canonical
// results live there regardless. An operator opts into a stable S3 publish
// location with --aws_outdir s3://<bucket>/out. Local and HealthOmics always
// publish (HealthOmics only exports its magic pubdir path).
func outdirConfig(target Target) string {
	if target == TargetAWSBatch {
		return `// Curated publish location for final outputs. Null by default: the canonical
// outputs already live in the S3 work dir (-work-dir s3://...); set
// --aws_outdir s3://<bucket>/out to also copy them to a stable S3 location.
params.aws_outdir = null
params.outdir = params.aws_outdir
`
	}

	return "params.outdir = '" + target.publishDir() + "'\n"
}

// configProcess renders the process{} block: the content-based retry strategy
// and, for container targets, a parameterized default image.
func configProcess(container string, target Target) string {
	assertExit := strconv.Itoa(shim.AssertExitCode)

	containerLines := ""
	if target.isContainer() {
		// Cloud backends run every task in a container; expose the image as a param
		// so an ECR URI can be supplied/validated at run time (required by
		// HealthOmics). Per-stage overrides go in withName: blocks.
		containerLines = "params.container = " + quoteOrNull(container) + "\n"
	}

	block := containerLines + `
// Content-based retry (mrp --autoretry analog): the shim exits ` + assertExit + ` for an
// ASSERT-class (non-retryable) stage failure and 1 for an ordinary (retryable)
// one, so terminate on ASSERT and retry everything else.
process {
    errorStrategy = { task.exitStatus == ` + assertExit + ` ? 'terminate' : 'retry' }
    maxRetries = 2
`
	switch {
	case target.isContainer():
		block += "    container = params.container\n"
	case container != "":
		block += "    container = '" + container + "'\n"
	}

	return block + "}\n"
}

// configProfiles renders the local + HPC executor profiles (the local target).
func configProfiles() string {
	return `
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

// configAWSBatch wires the AWS Batch executor with classic aws-CLI S3 staging.
//
//	Run with: nextflow run main.nf --aws_queue <q> --aws_region <r> \
//	  -work-dir s3://<bucket>/work --container <ecr-uri> [--aws_outdir s3://<bucket>/out]
func configAWSBatch() string {
	return `
// AWS Batch + S3 (classic aws-CLI staging). Requirements:
//  - workDir on S3:        -work-dir s3://<bucket>/work
//  - a container image:    --container <ecr-uri>  (see Dockerfile)
//  - the aws CLI present in the image, OR set --aws_cli_path for a custom AMI.
//  - (optional) a publish location: --aws_outdir s3://<bucket>/out (else the
//    canonical outputs remain in the S3 work dir only).
params.aws_cli_path = null
process.executor = 'awsbatch'
process.queue    = params.aws_queue
aws.region       = params.aws_region
aws.batch.cliPath = params.aws_cli_path
`
}

// configHealthOmics shapes the config for AWS HealthOmics private workflows:
// execution is fully managed (no executor/workDir), outputs go to the magic
// publishDir, and the Nextflow version is pinned to a supported release.
func configHealthOmics() string {
	return `
// AWS HealthOmics manages execution (instances, containers, the run filesystem),
// so no executor/workDir is set here. Outputs are exported from params.outdir
// (/mnt/workflow/pubdir). Containers must be private-ECR URIs (--container);
// tasks have no internet, so everything must be baked into the image.
manifest.nextflowVersion = '!>=23.10.0'
`
}

// quoteOrNull renders a Groovy string literal, or `null` when empty.
func quoteOrNull(s string) string {
	if s == "" {
		return "null"
	}

	return "'" + s + "'"
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	return nil
}
