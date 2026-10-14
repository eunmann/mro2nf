// Package cgnf emits a Nextflow project from Martian's compiled call graph
// (#463): one process per stage node of the witness invocation's
// call graph, each an alias of the one hand-written stage module, wired by
// the data the nodes exchange. A run-start PLAN task compiles the run's real
// invocation as mrp does and hands every node the run's call, its spec
// digest, its static gates and the forks its entry-fed roots take.
package cgnf

import (
	// The stage module ships inside the binary that emits it.
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/martian-lang/martian/martian/syntax"
)

//go:embed assets/stage.nf
var stageModule string

const (
	dirPerm  = 0o755
	filePerm = 0o644
	execPerm = 0o755
)

var (
	errReservedParam   = errors.New("entry input name is reserved by the generated project")
	errUndeclaredInput = errors.New("the call binds an input the pipeline does not declare")
	errNotStage        = errors.New("not a stage")
)

// reserved reports a param the generated project defines itself.
func reserved(param string) bool {
	return param == "outdir" || param == "job_resources" || strings.HasPrefix(param, "mro2nf_")
}

// Options are where a project is emitted and what it bakes in.
type Options struct {
	// OutDir receives the project.
	OutDir string
	// MroPaths are the absolute include paths the invocation compiles with.
	MroPaths []string
	// Version names the build in every task's cache key.
	Version string
}

// project is one emission: the source, the witness's call graph and its
// topology, the nodes that read entry files, and the entry inputs as run
// params, sorted.
type project struct {
	opts    Options
	src     cg.Source
	g       *cg.Graph
	topo    cg.Topology
	index   map[string]int
	roots   map[string]cg.TopoRoot
	readers map[string]bool
	inputs  []launchParam
}

// Emit writes the project for the entry .mro at entry into opts.OutDir.
func Emit(entry string, opts Options) error {
	src, err := cg.ReadSource(entry, opts.MroPaths)
	if err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	p, err := newProject(src, opts)
	if err != nil {
		return err
	}

	files, err := p.workflows()
	if err != nil {
		return err
	}

	schema, err := p.paramsSchema()
	if err != nil {
		return err
	}

	maps.Copy(files, map[string]string{
		"stage.nf":             stageModule,
		"nextflow.config":      p.config(),
		"nextflow_schema.json": schema,
	})

	if err := addMroFiles(files, src.Files); err != nil {
		return err
	}

	if err := p.addTargetFiles(files); err != nil {
		return err
	}

	for _, dir := range []string{mroAssets, subworkflowDir} {
		if err := os.RemoveAll(filepath.Join(opts.OutDir, dir)); err != nil {
			return fmt.Errorf("emit: %w", err)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := writeFile(filepath.Join(opts.OutDir, name), files[name], fileMode(name)); err != nil {
			return err
		}
	}

	return nil
}

// addTargetFiles adds what the container profiles need beside the
// project: their configs, and the HealthOmics package script and parameter
// template.
func (p *project) addTargetFiles(files map[string]string) error {
	tmpl, err := p.parameterTemplate()
	if err != nil {
		return err
	}

	maps.Copy(files, map[string]string{
		"conf/container.config":   containerConfig,
		"conf/healthomics.config": healthomicsConfig,
		"package.sh":              packageScript,
		"parameter-template.json": tmpl,
	})

	return nil
}

// fileMode is the mode a generated file is written with: package.sh is
// the one a user runs.
func fileMode(name string) os.FileMode {
	if name == "package.sh" {
		return execPerm
	}

	return filePerm
}

// mroAssets is the project's copy of the .mro files the entry compiles
// from, each at its absolute path below it, so their includes resolve as
// they do where the run finds its code. PLAN and OUTS compile it.
const mroAssets = "_assets/mro"

// addMroFiles adds each .mro file at its absolute path in paths to files,
// below mroAssets.
func addMroFiles(files map[string]string, paths []string) error {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("emit: %w", err)
		}

		files[filepath.Join(mroAssets, path)] = string(raw)
	}

	return nil
}

func newProject(src cg.Source, opts Options) (*project, error) {
	g, err := src.Witness(opts.MroPaths)
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	topo, err := g.Topology()
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	for id := range src.Invocation.Args {
		if reserved(id) {
			return nil, fmt.Errorf("emit: %w: %s", errReservedParam, id)
		}
	}

	readers, err := g.EntryFileReaders()
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	p := &project{opts: opts, src: src, g: g, topo: topo, index: map[string]int{}, roots: map[string]cg.TopoRoot{}, readers: readers}

	if p.inputs, err = p.entryInputs(); err != nil {
		return nil, err
	}

	for i, n := range topo.Nodes {
		p.index[n.Fqid] = i
	}

	for _, r := range topo.Roots {
		p.roots[r.Fqid] = r
	}

	return p, nil
}

// stage is the stage a node runs.
func (p *project) stage(fqid string) (*syntax.Stage, error) {
	st, err := p.g.Stage(fqid)
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	s, ok := st.Callable().(*syntax.Stage)
	if !ok {
		return nil, fmt.Errorf("emit %s: %w", fqid, errNotStage)
	}

	return s, nil
}

func writeFile(path, body string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}
