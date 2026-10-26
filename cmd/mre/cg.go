package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/martian-lang/martian/martian/core"
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
	fs.StringVar(&g.call, "call", "", "file holding the run's call, base64 of the gzip of the call mre plan rebuilt, in place of the entry's own (default: the entry's call)")
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

// readCall is the call the file at path holds (encodeCall); "" reads none.
// A task writes it from its value or stages the file the driver wrote, by
// the call's size: as an argument, a wide run's call would pass the
// kernel's limit on one argument (MAX_ARG_STRLEN, 128 KiB).
func readCall(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read -call: %w", err)
	}

	call, err := decodeCall(strings.TrimSpace(string(raw)))
	if err != nil {
		return "", fmt.Errorf("decode -call %s: %w", path, err)
	}

	return call, nil
}

// encodeCall is the run's call as every task gets it: base64 of its gzip.
// The call holds every entry value, a file path each, so it grows with the
// run, and a task gets all of it: 10,000 file paths are 1.67 MB of plain
// base64 per task, 37 KB gzipped. The gzip header records no name or time,
// so a PLAN re-run writes the same bytes and its tasks keep their -resume
// key.
func encodeCall(call string) (string, error) {
	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("compress call: %w", err)
	}

	if _, err := zw.Write([]byte(call)); err != nil {
		return "", fmt.Errorf("compress call: %w", err)
	}

	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("compress call: %w", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// decodeCall is the call text encodes (encodeCall).
func decodeCall(text string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", fmt.Errorf("decode call: %w", err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decompress call: %w", err)
	}

	call, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("decompress call: %w", err)
	}

	return string(call), nil
}

// argsFlags are the stage instance an args step resolves the args of.
type argsFlags struct {
	node, digest, leaves, values, refs string
	in, forks                          repeatedFlag
	roots                              shim.Roots
}

func (a *argsFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&a.node, "node", "", "stage fqid")
	fs.StringVar(&a.digest, "digest", "", "the node's spec digest in the run's plan, which it must compile to")
	fs.StringVar(&a.leaves, "leaves", "", "comma-separated indices of the run's entry file values the task staged, the i-th in entry/<i>/, from 1")
	fs.StringVar(&a.values, "values", "", "file of the upstream outputs and fork and partner elements the task got as values, whitespace-separated records a post wrote (-field-values, -fork-values)")
	fs.StringVar(&a.refs, "refs", "", "dir of the bundle leaves the task staged by reference, each with its ordinal dir at <n>/f/<ordinal>/, which the staged items name by origin (-field-refs, -fork-refs, -out-refs) where no staged item holds them")
	fs.Var(&a.in, "in", "staged item, or dir of staged items: instance outs bundles, fork elements and partner elements (repeatable)")
	fs.Var(&a.forks, "fork", "a fork record passed as a value, base64 of its JSON (repeatable)")
}

// readValues reads the value records in the file at path; "" reads
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

// write resolves the instance and its args from its staged upstream outs,
// the outs it got as values and its fork elements, its file leaves linked
// below out to the staged files, those named by reference among refs.
func (a *argsFlags) write(g *cg.Graph, out string, refs shim.Refs) (cg.Instance, core.LazyArgumentMap, error) {
	values, err := readValues(a.values)
	if err != nil {
		return cg.Instance{}, nil, fmt.Errorf("args: %w", err)
	}

	forks := make([]cg.Element, len(a.forks))
	for i, rec := range a.forks {
		el, err := g.DecodeElement(rec)
		if err != nil {
			return cg.Instance{}, nil, fmt.Errorf("args: %w", err)
		}

		forks[i] = el
	}

	files, err := stagedEntryFiles(g, a.leaves)
	if err != nil {
		return cg.Instance{}, nil, fmt.Errorf("args: %w", err)
	}

	spec := cg.ArgsSpec{Node: a.node, In: a.in, Values: values, Refs: refs, Forks: forks, Out: out, Log: os.Stderr, Digest: a.digest, EntryFiles: files, Roots: a.roots}

	inst, args, err := g.WriteArgs(spec)
	if err != nil {
		return cg.Instance{}, nil, fmt.Errorf("args: %w", err)
	}

	return inst, args, nil
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
	forks, forkValues, forkRefs, fields, values, fieldRefs, gates string
}

const refsUsage = ": space-separated records of the item, by its last two path components, the staged item a leaf it names by reference lies in, and the leaf's ordinal dir there, tab-separated"

func (p *postFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&p.forks, "forks", "forks", "dir to write <root>/<ordinal>/ fork elements and <root>@<node>.<output>/<index token>/ partner elements into, but for those -fork-values receives")
	fs.StringVar(&p.forkValues, "fork-values", "fork_values", "file to write the elements with no file leaf and small JSON into: space-separated records, each <dir>/<name>:<base64 of its JSON and record>, named as the dir below -forks it stands in for")
	fs.StringVar(&p.forkRefs, "fork-refs", "fork_refs", "file to write the references of the element dirs into"+refsUsage)
	fs.StringVar(&p.fields, "fields", "outs", "dir to write one stamped bundle per output into, named by it, but for those -field-values receives")
	fs.StringVar(&p.values, "field-values", "outs_values", "file to write the outputs with no file leaf and small JSON into: space-separated records, each <output>:<base64 of its JSON and instance>")
	fs.StringVar(&p.fieldRefs, "field-refs", "outs_refs", "file to write the references of the output bundles into"+refsUsage)
	fs.StringVar(&p.gates, "gates", "", "file to write the instances the instance disables into, space-separated gate tokens (<node>@<roots>@<ordinals>)")
}

// post posts the finished instance's outs, done, field by field, and
// writes the fork elements of every root it feeds and the gates it sets,
// naming by reference each file the outs name that is a whole leaf of a
// bundle in staged.
func (p *postFlags) post(g *cg.Graph, inst cg.Instance, done finished, staged shim.Staged) error {
	spec := cg.PostSpec{
		Instance: inst, Outs: done.outs, Opts: done.opts, Staged: staged,
		Forks: p.forks, ForkValues: p.forkValues, ForkRefs: p.forkRefs, Fields: p.fields, Values: p.values, FieldRefs: p.fieldRefs, Gates: p.gates,
	}
	if err := g.Post(spec); err != nil {
		return fmt.Errorf("post: %w", err)
	}

	return nil
}

// runOuts resolves the pipeline outs from the staged stage outs and
// publishes them as mrp does: the manifest of the published tree and the
// outs JSON naming it. It reads the staged outs' skeletons, never the
// bytes of a file a stage task wrote.
func runOuts(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("outs", flag.ContinueOnError)
	gf := addGraph(fs)
	manifest := fs.String("manifest", "publish.json", "file to write the published tree's entries to, each its path in the tree and where the task that wrote it keeps it; a file no stage task wrote is copied into external/ beside it")
	outsJSON := fs.String("json", "pipeline_outs.json", "file to write the outs JSON to")
	work := fs.String("work", "pipestance", "dir to reconstruct the pipestance in")
	locations := fs.String("locations", "", "file of tab-separated lines: a staged outs bundle, or a -refs <n> dir, and where its producing task's work dir keeps it")
	leaves := fs.String("leaves", "", "comma-separated indices of the run's entry file values the task staged, the i-th in entry/<i>/, from 1")
	valuesFile := fs.String("values", "", "file of the stage outputs the task got as values, whitespace-separated records a post wrote (-field-values)")
	refs := fs.String("refs", "", "dir of the bundles holding the leaves the task staged by reference, each at <n>/, which the staged outs name by origin where no staged outs bundle holds them; -locations locates each <n>")

	var in repeatedFlag

	fs.Var(&in, "in", "staged instance outs bundle, or dir of them (repeatable); a staged bundle skeleton (shim.SkeletonFile) is expanded in place")

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

	if err := expandSkeletons(append(slices.Clone(in), *refs)); err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	files, err := stagedEntryFiles(g, *leaves)
	if err != nil {
		return fmt.Errorf("outs: %w", err)
	}

	spec := cg.OutsSpec{In: in, Values: values, Refs: *refs, Locations: locs, EntryFiles: files, Work: *work, Manifest: *manifest, OutsJSON: *outsJSON}
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
