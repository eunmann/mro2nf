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

	if err := writeFile(filepath.Join(opts.OutDir, "nextflow.config"), []byte(configFile())); err != nil {
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

			spec := bindSpec(disableBindings(c))
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
		for _, c := range prog.Pipelines[name].Calls {
			if !c.Mapped {
				continue
			}

			if c.Disabled != nil {
				return &apperror.UnsupportedError{
					Construct: "disabled map call",
					Detail:    name + "." + c.Name,
				}
			}

			if s, ok := prog.Stages[c.Callable]; !ok || s.Split {
				return &apperror.UnsupportedError{
					Construct: "map call over a split stage or pipeline",
					Detail:    name + "." + c.Name + " -> " + c.Callable,
				}
			}
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
			if err := writeSpec(specDir, bindName(p.Name, c.Name), c.Bindings); err != nil {
				return err
			}
		}

		if err := writeSpec(specDir, bindName(p.Name, "return"), p.Returns); err != nil {
			return err
		}
	}

	return nil
}

func writeSpec(specDir, name string, bindings []ir.Binding) error {
	data, err := json.MarshalIndent(bindSpec(bindings), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spec %s: %w", name, err)
	}

	return writeFile(filepath.Join(specDir, name+".json"), data)
}

// writeEntryArgs resolves the top-level call's inputs and writes them as the
// entry_args bundle, staging any file-typed entry inputs so the run is
// self-contained from the start.
func writeEntryArgs(prog *ir.Program, outDir string) error {
	args, err := bind.Resolve(bindSpec(prog.Entry.Bindings), nil, nil)
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
// bindings (handled separately).
func bindSpec(bindings []ir.Binding) bind.Spec {
	spec := bind.Spec{}

	for _, b := range bindings {
		if b.Param == "*" {
			continue
		}

		entry := valueToEntry(b.Value)
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
func valueToEntry(v ir.Value) bind.Entry {
	switch {
	case v.Array != nil:
		arr := make([]bind.Entry, len(v.Array))
		for i, e := range v.Array {
			arr[i] = valueToEntry(e)
		}

		return bind.Entry{Array: arr}
	case v.Object != nil:
		obj := make(map[string]bind.Entry, len(v.Object))
		for k, e := range v.Object {
			obj[k] = valueToEntry(e)
		}

		return bind.Entry{Object: obj}
	case v.Ref != nil:
		return bind.Entry{Ref: &bind.Ref{Kind: v.Ref.Kind, ID: v.Ref.ID, Output: v.Ref.Output}}
	default:
		return bind.Entry{Literal: v.Literal}
	}
}

func configFile() string {
	return `params.outdir = 'results'

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
