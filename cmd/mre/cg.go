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
	"github.com/eunmann/mro2nf/internal/shim"
)

var errNoEntry = errors.New("-entry is required")

// graphFlags locate the invocation a call-graph subcommand resolves against,
// whose compiled types its bundles are written with.
type graphFlags struct {
	entry, call, mropath string
}

func addGraph(fs *flag.FlagSet) *graphFlags {
	g := &graphFlags{}
	fs.StringVar(&g.entry, "entry", "", "the entry .mro, compiled as mrp compiles it; Martian resolves its includes and stage code beside it")
	fs.StringVar(&g.call, "call", "", "file holding the run's call, base64 of the call mre plan rebuilt, in place of the entry's own (default: the entry's call)")
	fs.StringVar(&g.mropath, "mropath", "", "colon-separated include path (default: the -entry file's dir)")

	return g
}

func (g *graphFlags) load() (*cg.Graph, error) {
	if g.entry == "" {
		return nil, errNoEntry
	}

	var paths []string
	if g.mropath != "" {
		paths = filepath.SplitList(g.mropath)
	}

	call, err := readCall(g.call)
	if err != nil {
		return nil, err
	}

	graph, err := cg.Load(g.entry, call, paths)
	if err != nil {
		return nil, fmt.Errorf("load call graph: %w", err)
	}

	return graph, nil
}

// readCall is the call the file at path holds, base64; "" reads none. A
// task stages the file the driver wrote: as an argument, a wide run's call
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
	node, digest, leaves, values string
	in, forks                    repeatedFlag
	roots                        shim.Roots
}

func (a *argsFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&a.node, "node", "", "stage fqid")
	fs.StringVar(&a.digest, "digest", "", "the node's spec digest in the run's plan, which it must compile to")
	fs.StringVar(&a.leaves, "leaves", "", "comma-separated indices of the run's entry file values the task staged, the i-th in entry/<i>/, from 1")
	fs.StringVar(&a.values, "values", "", "file of the upstream outputs the task got as values, whitespace-separated records a post wrote (-field-values)")
	fs.Var(&a.in, "in", "staged item, or dir of staged items: instance outs bundles and fork elements (repeatable)")
	fs.Var(&a.forks, "fork", "a fork record passed as a value, base64 of its JSON (repeatable)")
}

// readValues reads the outs value records in the file at path; "" reads
// none. A task writes them into a file, as a gather's could pass the
// kernel's limit on a command line.
func readValues(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read -values: %w", err)
	}

	return strings.Fields(string(raw)), nil
}

// write writes the instance's args bundle into out from its staged upstream
// outs, the outs it got as values and its fork elements, its file leaves
// linked to the staged files.
func (a *argsFlags) write(g *cg.Graph, out string) (cg.Instance, error) {
	values, err := readValues(a.values)
	if err != nil {
		return cg.Instance{}, fmt.Errorf("args: %w", err)
	}

	forks := make([]cg.Element, len(a.forks))
	for i, rec := range a.forks {
		el, err := g.DecodeElement(rec)
		if err != nil {
			return cg.Instance{}, fmt.Errorf("args: %w", err)
		}

		forks[i] = el
	}

	files, err := stagedEntryFiles(g, a.leaves)
	if err != nil {
		return cg.Instance{}, fmt.Errorf("args: %w", err)
	}

	inst, err := g.WriteArgs(cg.ArgsSpec{Node: a.node, In: a.in, Values: values, Forks: forks, Out: out, Log: os.Stderr, Digest: a.digest, EntryFiles: files, Roots: a.roots})
	if err != nil {
		return cg.Instance{}, fmt.Errorf("args: %w", err)
	}

	return inst, nil
}

// stagedEntryFiles maps the entry file values -leaves lists, which the
// task staged below entry/, to the paths it reads them at.
func stagedEntryFiles(g *cg.Graph, leaves string) (map[string]string, error) {
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

	files, err := g.StagedEntryFiles("entry", ids)
	if err != nil {
		return nil, fmt.Errorf("staged entry files: %w", err)
	}

	return files, nil
}

// postFlags are where a post step writes what a finished instance feeds.
type postFlags struct {
	forks, fields, values, gates string
	roots                        shim.Roots
}

func (p *postFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&p.forks, "forks", "forks", "dir to write <root>/<ordinal>/ fork elements into")
	fs.StringVar(&p.fields, "fields", "", "dir to write one stamped bundle per output into, named by it, but for those -field-values receives")
	fs.StringVar(&p.values, "field-values", "", "file to write the outputs with no file leaf and small JSON into, set with -fields: space-separated records, each <output>:<base64 of its JSON and instance>")
	fs.StringVar(&p.gates, "gates", "", "file to write the instances the instance disables into, space-separated gate tokens (<node>@<roots>@<ordinals>)")
}

// post stamps the outs bundle outs of the instance the args bundle args
// stamps and writes the fork elements of every root it feeds.
func (p *postFlags) post(g *cg.Graph, args, outs string) error {
	if err := g.Post(cg.PostSpec{Args: args, Outs: outs, Forks: p.forks, Fields: p.fields, Values: p.values, Gates: p.gates, Roots: p.roots}); err != nil {
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
	valuesFile := fs.String("values", "", "file of the stage outputs the task got as values, whitespace-separated records a post wrote (-field-values)")

	var in repeatedFlag

	fs.Var(&in, "in", "staged instance outs bundle, or dir of them (repeatable)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, err := gf.load()
	if err != nil {
		return err
	}

	values, err := readValues(*valuesFile)
	if err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	locs, err := readLocations(*locations)
	if err != nil {
		return err
	}

	files, err := stagedEntryFiles(g, *leaves)
	if err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	spec := cg.OutsSpec{In: in, Values: values, Locations: locs, EntryFiles: files, Work: *work, Results: *out, OutsJSON: *outsJSON}
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
