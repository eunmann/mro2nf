package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/types"
)

var errNoEntry = errors.New("-entry is required")

// graphFlags locate the invocation a call-graph subcommand resolves against
// and the type manifest its bundles are written with.
type graphFlags struct {
	entry, call, mropath, types string
}

func addGraph(fs *flag.FlagSet) *graphFlags {
	g := &graphFlags{}
	fs.StringVar(&g.entry, "entry", "", "the entry .mro, compiled as mrp compiles it; Martian resolves its includes and stage code beside it")
	fs.StringVar(&g.call, "call", "", "the run's call, base64 of the call mre plan rebuilt, in place of the entry's own (default: the entry's call)")
	fs.StringVar(&g.mropath, "mropath", "", "colon-separated include path (default: the -entry file's dir)")
	fs.StringVar(&g.types, "types", "", "type manifest (types.json) path (default: built from the invocation's declarations)")

	return g
}

func (g *graphFlags) load() (*cg.Graph, types.Manifest, error) {
	if g.entry == "" {
		return nil, types.Manifest{}, errNoEntry
	}

	var paths []string
	if g.mropath != "" {
		paths = filepath.SplitList(g.mropath)
	}

	call, err := base64.StdEncoding.DecodeString(g.call)
	if err != nil {
		return nil, types.Manifest{}, fmt.Errorf("decode -call: %w", err)
	}

	graph, err := cg.Load(g.entry, string(call), paths)
	if err != nil {
		return nil, types.Manifest{}, fmt.Errorf("load call graph: %w", err)
	}

	if g.types == "" {
		return graph, graph.Manifest(), nil
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
	digest := fs.String("digest", "", "the node's spec digest in the run's plan, which it must compile to")
	out := fs.String("o", "args", "args bundle dir to write")

	var in, forkRecs repeatedFlag

	fs.Var(&in, "in", "staged item, or dir of staged items: instance outs bundles and fork elements (repeatable)")
	fs.Var(&forkRecs, "fork", "a fork record passed as a value, base64 of its JSON (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	forks := make([]cg.Element, len(forkRecs))
	for i, rec := range forkRecs {
		if forks[i], err = g.DecodeElement(rec); err != nil {
			return fmt.Errorf("args: %w", err)
		}
	}

	inst, err := g.WriteArgs(cg.ArgsSpec{Node: *node, In: in, Forks: forks, Manifest: man, Out: *out, Log: os.Stderr, Digest: *digest})
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
// every root it feeds; -entry-forks writes those of the roots no stage
// feeds.
func runPost(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	gf := addGraph(fs)
	args := fs.String("args", "args", "the instance's args bundle dir")
	outs := fs.String("outs", "outs", "the stage's outs bundle dir")
	forks := fs.String("forks", "forks", "dir to write <root>/<ordinal>/ fork elements into")
	entry := fs.Bool("entry-forks", false, "write the forks the invocation's entry values fix, not a stage's")
	fields := fs.String("fields", "", "dir to write one stamped bundle per output into, named by it")
	gates := fs.String("gates", "", "file to write the instances the instance disables into, space-separated gate tokens (<node>@<roots>@<ordinals>)")

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
		err = g.Post(cg.PostSpec{Args: *args, Outs: *outs, In: in, Manifest: man, Forks: *forks, Fields: *fields, Gates: *gates})
	}

	if err != nil {
		return fmt.Errorf("post: %w", err)
	}

	return nil
}

// runOuts resolves the pipeline outs from the staged stage outs and
// publishes them as mrp does: the published tree and the outs JSON naming it.
func runOuts(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("outs", flag.ContinueOnError)
	gf := addGraph(fs)
	out := fs.String("o", "results", "dir to write the published tree into")
	outsJSON := fs.String("json", "pipeline_outs.json", "file to write the outs JSON to")
	work := fs.String("work", "pipestance", "dir to reconstruct the pipestance in")
	locations := fs.String("locations", "", "file of tab-separated lines: a staged outs bundle, and where its producing task's work dir keeps it")

	var in repeatedFlag

	fs.Var(&in, "in", "staged instance outs bundle, or dir of them (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	locs, err := readLocations(*locations)
	if err != nil {
		return err
	}

	spec := cg.OutsSpec{In: in, Locations: locs, Manifest: man, Work: *work, Results: *out, OutsJSON: *outsJSON}
	if err := g.WriteOuts(ctx, spec); err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	return nil
}

// errBadLocation reports a locations line that is not a bundle and a
// location separated by a tab.
var errBadLocation = errors.New("malformed location line")

// readLocations reads the locations file at path into a map from each
// bundle's absolute path to its location; "" reads none.
func readLocations(path string) (map[string]string, error) {
	locs := map[string]string{}
	if path == "" {
		return locs, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read locations: %w", err)
	}

	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}

		bundle, loc, ok := strings.Cut(line, "\t")
		if !ok || bundle == "" || loc == "" {
			return nil, fmt.Errorf("%w: %q", errBadLocation, line)
		}

		abs, err := filepath.Abs(bundle)
		if err != nil {
			return nil, fmt.Errorf("read locations: %w", err)
		}

		locs[abs] = loc
	}

	return locs, nil
}
