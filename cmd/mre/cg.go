package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/types"
)

var errNoMRO = errors.New("-mro is required")

// graphFlags locate the invocation a call-graph subcommand resolves against
// and the type manifest its bundles are written with.
type graphFlags struct {
	mro, mropath, types string
}

func addGraph(fs *flag.FlagSet) *graphFlags {
	g := &graphFlags{}
	fs.StringVar(&g.mro, "mro", "", "the invocation .mro, compiled as mrp compiles it")
	fs.StringVar(&g.mropath, "mropath", "", "colon-separated include path (default: the .mro's dir)")
	fs.StringVar(&g.types, "types", "", "type manifest (types.json) path")

	return g
}

func (g *graphFlags) load() (*cg.Graph, types.Manifest, error) {
	if g.mro == "" {
		return nil, types.Manifest{}, errNoMRO
	}

	var paths []string
	if g.mropath != "" {
		paths = filepath.SplitList(g.mropath)
	}

	graph, err := cg.Load(g.mro, paths)
	if err != nil {
		return nil, types.Manifest{}, fmt.Errorf("load call graph: %w", err)
	}

	if g.types == "" {
		return graph, types.Manifest{}, nil
	}

	man, err := types.LoadManifest(g.types)
	if err != nil {
		return nil, types.Manifest{}, fmt.Errorf("load type manifest: %w", err)
	}

	return graph, man, nil
}

// runArgs writes one stage instance's args bundle from its staged upstream
// outs and fork elements, and prints the instance's state, enabled or
// disabled, on stdout.
func runArgs(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("args", flag.ContinueOnError)
	gf := addGraph(fs)
	node := fs.String("node", "", "stage fqid")
	out := fs.String("o", "args", "args bundle dir to write")

	var in repeatedFlag

	fs.Var(&in, "in", "staged item, or dir of staged items: instance outs bundles and fork elements (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	inst, err := g.WriteArgs(cg.ArgsSpec{Node: *node, In: in, Manifest: man, Out: *out, Log: os.Stderr})
	if err != nil {
		return fmt.Errorf("args: %w", err)
	}

	state := "enabled"
	if inst.Disabled {
		state = "disabled"
	}

	if _, err := fmt.Fprintln(os.Stdout, state); err != nil {
		return fmt.Errorf("write state: %w", err)
	}

	return nil
}

// runPost stamps a finished instance's outs and writes the fork elements of
// every root it feeds; -entry writes those of the roots no stage feeds.
func runPost(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	gf := addGraph(fs)
	args := fs.String("args", "args", "the instance's args bundle dir")
	outs := fs.String("outs", "outs", "the stage's outs bundle dir")
	forks := fs.String("forks", "forks", "dir to write <root>/<ordinal>/ fork elements into")
	entry := fs.Bool("entry", false, "write the forks the invocation's entry values fix, not a stage's")

	var in repeatedFlag

	fs.Var(&in, "in", "staged item a fork source also reads (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	if *entry {
		err = g.PostEntry(*forks)
	} else {
		err = g.Post(cg.PostSpec{Args: *args, Outs: *outs, In: in, Manifest: man, Forks: *forks})
	}

	if err != nil {
		return fmt.Errorf("post: %w", err)
	}

	return nil
}

// runOuts resolves the pipeline outs from the staged stage outs into the
// bundle publish-layout reads.
func runOuts(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("outs", flag.ContinueOnError)
	gf := addGraph(fs)
	out := fs.String("o", "outs", "pipeline outs bundle dir to write")

	var in repeatedFlag

	fs.Var(&in, "in", "staged instance outs bundle, or dir of them (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	if err := g.WriteOuts(cg.OutsSpec{In: in, Manifest: man, Out: *out}); err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	return nil
}
