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
	errReservedParam = errors.New("entry input name is reserved by the generated project")
	errNotStage      = errors.New("not a stage")
)

// reserved reports a param the generated project defines itself.
func reserved(param string) bool {
	return param == "outdir" || param == "job_resources" || strings.HasPrefix(param, "mro2nf_")
}

// Options are the paths and knobs a generated project bakes in.
type Options struct {
	// OutDir receives the project.
	OutDir string
	// Mre, Mrjob and Adapters are the absolute paths of the mre binary, the
	// mrjob py and comp stages run under, and Martian's adapters dir.
	Mre, Mrjob, Adapters string
	// MroPaths are the absolute include paths the invocation compiles with.
	MroPaths []string
	// Monitor is mrp's --monitor.
	Monitor bool
	// Confine fails a stage task whose output resolves outside its work
	// dir instead of copying it in (mre run -confine).
	Confine bool
	// Version names the build in every task's cache key.
	Version string
}

// project is one emission: the source, the witness's call graph and its
// topology, and the nodes that read entry files.
type project struct {
	opts    Options
	src     cg.Source
	g       *cg.Graph
	topo    cg.Topology
	index   map[string]int
	roots   map[string]cg.TopoRoot
	entry   []string
	readers map[string]bool
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

	man, err := src.Manifest(opts.MroPaths)
	if err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	types, err := man.Encode()
	if err != nil {
		return fmt.Errorf("emit types: %w", err)
	}

	main, err := p.mainNF()
	if err != nil {
		return err
	}

	files := map[string]string{
		"main.nf":            main,
		"stage.nf":           stageModule,
		"nextflow.config":    p.config(),
		"_assets/types.json": types.Subset(slices.Sorted(maps.Keys(man.Callables))...),
	}

	if err := addMroFiles(files, src.Files); err != nil {
		return err
	}

	if err := p.addTargetFiles(files); err != nil {
		return err
	}

	if err := os.RemoveAll(filepath.Join(opts.OutDir, mroAssets)); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := writeFile(filepath.Join(opts.OutDir, name), files[name], fileMode(name)); err != nil {
			return err
		}
	}

	return nil
}

// addTargetFiles adds what the container profiles need beside the
// project: their configs, the stage image's recipe and its build script,
// and the HealthOmics package script and parameter template.
func (p *project) addTargetFiles(files map[string]string) error {
	roots, commands, err := p.codeRoots()
	if err != nil {
		return err
	}

	tmpl, err := p.parameterTemplate()
	if err != nil {
		return err
	}

	maps.Copy(files, map[string]string{
		"conf/container.config":   containerConfig(),
		"conf/healthomics.config": healthomicsConfig,
		"Dockerfile":              p.dockerfile(roots, commands),
		".dockerignore":           dockerignore,
		"build.sh":                buildScript(roots),
		"package.sh":              packageScript,
		"parameter-template.json": tmpl,
	})

	return nil
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

	readers, err := g.EntryFileReaders(g.Manifest())
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	p := &project{opts: opts, src: src, g: g, topo: topo, index: map[string]int{}, roots: map[string]cg.TopoRoot{}, readers: readers}

	for i, n := range topo.Nodes {
		p.index[n.Fqid] = i
	}

	for _, r := range topo.Roots {
		p.roots[r.Fqid] = r
		if r.Producer == "" {
			p.entry = append(p.entry, r.Fqid)
		}
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
