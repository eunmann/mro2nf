package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	fs.StringVar(&g.call, "call", "", "file holding the run's call, base64 of the call mre plan rebuilt, in place of the entry's own (default: the entry's call)")
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

	call, err := readCall(g.call)
	if err != nil {
		return nil, types.Manifest{}, err
	}

	graph, err := cg.Load(g.entry, call, paths)
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

// readCall is the call the file at path holds, base64; "" reads none. A
// task writes it from its own value: as an argument, a wide run's call
// would pass the kernel's limit on one argument (MAX_ARG_STRLEN, 128 KiB).
func readCall(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read -call: %w", err)
	}

	call, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", fmt.Errorf("decode -call %s: %w", path, err)
	}

	return string(call), nil
}

// argsFlags are the stage instance an args step builds the args bundle of.
type argsFlags struct {
	node, digest, leaves string
	in, forks            repeatedFlag
}

func (a *argsFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&a.node, "node", "", "stage fqid")
	fs.StringVar(&a.digest, "digest", "", "the node's spec digest in the run's plan, which it must compile to")
	fs.StringVar(&a.leaves, "leaves", "", "comma-separated indices of the run's entry file values the task staged, the i-th in entry/<i>/, from 1")
	fs.Var(&a.in, "in", "staged item, or dir of staged items: instance outs bundles and fork elements (repeatable)")
	fs.Var(&a.forks, "fork", "a fork record passed as a value, base64 of its JSON (repeatable)")
}

// write writes the instance's args bundle into out from its staged upstream
// outs and fork elements, its file leaves linked to the staged files when
// link is set.
func (a *argsFlags) write(g *cg.Graph, man types.Manifest, out string, link bool) (cg.Instance, error) {
	forks := make([]cg.Element, len(a.forks))
	for i, rec := range a.forks {
		el, err := g.DecodeElement(rec)
		if err != nil {
			return cg.Instance{}, fmt.Errorf("args: %w", err)
		}

		forks[i] = el
	}

	files, err := stagedEntryFiles(g, man, a.leaves)
	if err != nil {
		return cg.Instance{}, fmt.Errorf("args: %w", err)
	}

	inst, err := g.WriteArgs(cg.ArgsSpec{Node: a.node, In: a.in, Forks: forks, Manifest: man, Out: out, Log: os.Stderr, Digest: a.digest, EntryFiles: files, Link: link})
	if err != nil {
		return cg.Instance{}, fmt.Errorf("args: %w", err)
	}

	return inst, nil
}

// runArgs writes one stage instance's args bundle and prints the instance's
// state, enabled or disabled, on stdout.
func runArgs(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("args", flag.ContinueOnError)
	gf := addGraph(fs)

	var a argsFlags

	a.register(fs)
	out := fs.String("o", "args", "args bundle dir to write")
	link := fs.Bool("link", false, "no other task stages the bundle: link its file leaves to the staged files instead of copying them")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	inst, err := a.write(g, man, *out, *link)
	if err != nil {
		return err
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

// stagedEntryFiles maps the entry file values -leaves lists, which the
// task staged below entry/, to the paths it reads them at.
func stagedEntryFiles(g *cg.Graph, man types.Manifest, leaves string) (map[string]string, error) {
	if leaves == "" {
		return nil, nil
	}

	var ids []int

	for f := range strings.SplitSeq(leaves, ",") {
		id, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("-leaves: %w", err)
		}

		ids = append(ids, id)
	}

	files, err := g.StagedEntryFiles(man, "entry", ids)
	if err != nil {
		return nil, fmt.Errorf("staged entry files: %w", err)
	}

	return files, nil
}

// postFlags are where a post step writes what a finished instance feeds.
type postFlags struct {
	forks, fields, gates string
}

func (p *postFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&p.forks, "forks", "forks", "dir to write <root>/<ordinal>/ fork elements into")
	fs.StringVar(&p.fields, "fields", "", "dir to write one stamped bundle per output into, named by it")
	fs.StringVar(&p.gates, "gates", "", "file to write the instances the instance disables into, space-separated gate tokens (<node>@<roots>@<ordinals>)")
}

// post stamps the outs bundle outs of the instance the args bundle args
// stamps and writes the fork elements of every root it feeds; in are the
// staged items a fork source also reads.
func (p *postFlags) post(g *cg.Graph, man types.Manifest, args, outs string, in []string) error {
	if err := g.Post(cg.PostSpec{Args: args, Outs: outs, In: in, Manifest: man, Forks: p.forks, Fields: p.fields, Gates: p.gates}); err != nil {
		return fmt.Errorf("post: %w", err)
	}

	return nil
}

// runPost stamps a finished instance's outs and writes the fork elements of
// every root it feeds; -entry-forks writes those of the roots no stage
// feeds.
func runPost(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	gf := addGraph(fs)

	var p postFlags

	p.register(fs)
	args := fs.String("args", "args", "the instance's args bundle dir")
	outs := fs.String("outs", "outs", "the stage's outs bundle dir")
	entry := fs.Bool("entry-forks", false, "write the forks the invocation's entry values fix, not a stage's")

	var in repeatedFlag

	fs.Var(&in, "in", "staged item a fork source also reads (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	if !*entry {
		return p.post(g, man, *args, *outs, in)
	}

	if err := g.PostEntry(p.forks); err != nil {
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
	leaves := fs.String("leaves", "", "comma-separated indices of the run's entry file values the task staged, the i-th in entry/<i>/, from 1")

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

	files, err := stagedEntryFiles(g, man, *leaves)
	if err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	spec := cg.OutsSpec{In: in, Locations: locs, EntryFiles: files, Manifest: man, Work: *work, Results: *out, OutsJSON: *outsJSON}
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
