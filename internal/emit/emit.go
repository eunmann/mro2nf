// Package emit renders a transpiler IR program into a runnable Nextflow
// project: main.nf, nextflow.config, per-call binding specs, and the entry
// args. Each generated process invokes the mre shim against the original
// Martian stage code.
package emit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eunmann/martian-nextflow/internal/bind"
	"github.com/eunmann/martian-nextflow/internal/ir"
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

	specDir := filepath.Join(opts.OutDir, "bindspecs")
	if err := os.MkdirAll(specDir, dirPerm); err != nil {
		return fmt.Errorf("create output dirs: %w", err)
	}

	g := genCtx{
		entry:   prog.Entry.Callable,
		mroFile: opts.MROFile,
		mre:     opts.Mre,
		shell:   opts.Shell,
		code:    opts.StageCode,
	}

	if err := writeFile(filepath.Join(opts.OutDir, "main.nf"), []byte(generateNF(prog, g))); err != nil {
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

			spec := bind.Spec{"disabled": {Ref: &bind.Ref{
				Kind: c.Disabled.Kind, ID: c.Disabled.ID, Output: c.Disabled.Output,
			}}}
			if err := writeJSONFile(filepath.Join(specDir, disableName(p.Name, c.Name)+".json"), spec); err != nil {
				return err
			}

			nulls := nullOuts(prog, c.Callable)
			if err := writeJSONFile(filepath.Join(nullsDir, p.Name+"__"+c.Name+".json"), nulls); err != nil {
				return err
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

func writeEntryArgs(prog *ir.Program, outDir string) error {
	args, err := bind.Resolve(bindSpec(prog.Entry.Bindings), nil, nil)
	if err != nil {
		return fmt.Errorf("resolve entry args: %w", err)
	}

	return writeFile(filepath.Join(outDir, "entry_args.json"), args)
}

// bindSpec converts IR bindings into a runtime binding spec, skipping wildcard
// bindings (handled separately).
func bindSpec(bindings []ir.Binding) bind.Spec {
	spec := bind.Spec{}

	for _, b := range bindings {
		if b.Param == "*" {
			continue
		}

		entry := bind.Entry{Split: b.Split}
		if b.Ref != nil {
			entry.Ref = &bind.Ref{Kind: b.Ref.Kind, ID: b.Ref.ID, Output: b.Ref.Output}
		} else {
			entry.Literal = b.Literal
		}

		spec[b.Param] = entry
	}

	return spec
}

func configFile() string {
	return "params.outdir = 'results'\n"
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	return nil
}
