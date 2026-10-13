package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/util"
)

// planFlags are `mre plan`'s inputs and outputs.
type planFlags struct {
	entry, values, mropath, call, nodes, roots, leaves, files, work string
	overrides, psid, s3Mount                                        string
	hostInputs, remoteInputs                                        bool
	local                                                           job.Pool
}

func (f *planFlags) define(fs *flag.FlagSet) {
	fs.StringVar(&f.entry, "entry", "", "the transpiled entry .mro")
	fs.StringVar(&f.values, "values", "", "JSON file of {input: value} entry values; a null keeps the transpiled one")
	fs.StringVar(&f.mropath, "mropath", "", "colon-separated include path")
	fs.StringVar(&f.overrides, "overrides", "", "the run's mrp --overrides file")
	fs.StringVar(&f.psid, "psid", "", "the pipestance id, whose fqnames the overrides walk, as mrp requires it: letters, digits, '-' and '_'")
	fs.StringVar(&f.call, "call", "call", "file to write the run's call into, base64 of the call as mrp's invocation generator builds it")
	fs.StringVar(&f.nodes, "nodes", "nodes", "file to write one line per witness node into: fqid digest off disabled-keys cpus mem-gb overrides")
	fs.StringVar(&f.roots, "roots", "roots", "file to write one line per fork of every entry-fed fork root into: root key record index-token")
	fs.StringVar(&f.leaves, "leaves", "leaves", "file to write one line per entry file value a task stages into: index base64-value")
	fs.StringVar(&f.files, "files", "files", "file to write one line per instance group that stages entry files into: fqid fork-key indices")
	fs.BoolVar(&f.hostInputs, "host-inputs", true, "an entry file value may name a head-node path")
	fs.BoolVar(&f.remoteInputs, "remote-inputs", false, "an entry file value may name a URI of any scheme, not only s3")
	fs.StringVar(&f.s3Mount, "s3-mount", "", "the root every task may mount input buckets under, read-only: an s3://<bucket>/<key> entry file value whose bucket is a directory there is <root>/<bucket>/<key>, which no task stages; any other is staged")
	fs.StringVar(&f.work, "work", ".", "scratch dir")
	definePool(fs, &f.local)
}

// runPlan starts a run: it rebuilds the invocation from the transpiled
// entry and the run's entry values as mrp builds an invocation from its
// JSON form, compiles it as mrp does, so mrp's refusals fail the run before
// any stage starts, and checks its call graph is within the witness's, and
// that every entry file value is one the run may stage, or, an s3:// one
// of a bucket the -s3-mount root shows, one the mount shows at the path the
// call names it by.
// It writes the call every task compiles, each witness node's spec digest,
// disabled instances and entry files, and the forks of each entry-fed root.
func runPlan(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)

	var f planFlags

	f.define(fs)

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if f.entry == "" {
		return errNoEntry
	}

	if err := util.ValidateID(f.psid); err != nil {
		return fmt.Errorf("plan: psid: %w", err)
	}

	if err := f.local.Check(); err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	over, err := readOverrides(f.overrides)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	mount, err := shim.ParseMount(f.s3Mount)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	paths := filepath.SplitList(f.mropath)

	src, err := cg.ReadSource(f.entry, paths)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	inv, err := overlayInvocation(src, f.values, paths)
	if err != nil {
		return err
	}

	if inv, err = cg.MountEntryValues(src, inv, paths, mount); err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	p, err := cg.MakePlan(src, inv, paths, f.work, mount)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	if err := f.vetLeaves(p.Leaves, mount); err != nil {
		return err
	}

	if err := f.writeEntryFiles(p); err != nil {
		return err
	}

	return f.write(p, job.Runtime{PSID: f.psid, Overrides: over, Local: f.local})
}

// vetLeaves refuses an entry file value the run may not read: a mounted one
// the mount does not show, any other one the run may not stage.
func (f *planFlags) vetLeaves(leaves []cg.EntryLeaf, mount shim.Mount) error {
	for _, l := range leaves {
		vet := cg.VetEntryLeaf(l, f.hostInputs, f.remoteInputs)
		if l.Mounted {
			vet = cg.VetMountedLeaf(l, mount)
		}

		if vet != nil {
			return fmt.Errorf("plan: %w", vet)
		}
	}

	return nil
}

// writeEntryFiles writes the run's entry file values a task stages, each by
// its index, and per node and fork of its entry-fed roots, those its
// instance there stages, and by the pipeline's fqid, those OUTS stages.
func (f *planFlags) writeEntryFiles(p cg.Plan) error {
	var leaves, files strings.Builder

	for i, l := range p.Leaves {
		if !l.Mounted {
			fmt.Fprintf(&leaves, "%d %s\n", i, base64.StdEncoding.EncodeToString([]byte(l.Value)))
		}
	}

	for _, n := range append(slices.Clone(p.Nodes), p.Outs) {
		for _, pf := range n.Files {
			ids := make([]string, len(pf.Leaves))
			for i, x := range pf.Leaves {
				ids[i] = strconv.Itoa(x)
			}

			fmt.Fprintf(&files, "%s %s %s\n", n.Fqid, orDash(pf.Key), strings.Join(ids, ","))
		}
	}

	if err := writeText(f.leaves, leaves.String()); err != nil {
		return err
	}

	return writeText(f.files, files.String())
}

// overlayInvocation is the source's invocation with the run's entry values
// over its own: a string for a scalar input parsed as its declared type, as
// a command-line param is a string.
func overlayInvocation(src cg.Source, valuesFile string, mroPaths []string) (*core.InvocationData, error) {
	inv := *src.Invocation
	inv.Args = maps.Clone(src.Invocation.Args)

	if valuesFile == "" {
		return &inv, nil
	}

	raw, err := os.ReadFile(valuesFile)
	if err != nil {
		return nil, fmt.Errorf("read values: %w", err)
	}

	over, err := rawToMap(raw)
	if err != nil {
		return nil, fmt.Errorf("decode values: %w", err)
	}

	params, tbl, err := src.InParams(inv.Call, mroPaths)
	if err != nil {
		return nil, fmt.Errorf("plan: entry params: %w", err)
	}

	if err := typeOverrides(over, params, tbl); err != nil {
		return nil, err
	}

	for _, k := range slices.Sorted(maps.Keys(over)) {
		if over[k] == nil {
			continue
		}

		v, err := json.Marshal(types.Sorted(over[k]))
		if err != nil {
			return nil, fmt.Errorf("encode entry value %s: %w", k, err)
		}

		inv.Args[k] = v
	}

	return &inv, nil
}

func (f *planFlags) write(p cg.Plan, rt job.Runtime) error {
	if err := writeText(f.call, base64.StdEncoding.EncodeToString([]byte(p.Call))); err != nil {
		return err
	}

	var nodes strings.Builder

	for _, n := range p.Nodes {
		off, at := "0", "-"
		if n.Off {
			off = "1"
		}

		if len(n.OffAt) > 0 {
			at = strings.Join(n.OffAt, ",")
		}

		res, err := nodeResources(rt, n)
		if err != nil {
			return err
		}

		fmt.Fprintf(&nodes, "%s %s %s %s %s\n", n.Fqid, orDash(n.Digest), off, at, res)
	}

	if err := writeText(f.nodes, nodes.String()); err != nil {
		return err
	}

	return f.writeRoots(p.Roots)
}

// writeRoots writes the forks of every entry-fed root as one record
// whatever the number of roots, so PLAN's outputs and the driver's wiring
// of them stay fixed; each line leads with its root.
func (f *planFlags) writeRoots(roots map[string][]cg.PlanFork) error {
	var forks strings.Builder

	for _, root := range slices.Sorted(maps.Keys(roots)) {
		for _, pf := range roots[root] {
			fmt.Fprintf(&forks, "%s %s %s %s\n", root, cg.DottedKey(pf.Key), pf.Record, pf.Token)
		}
	}

	return writeText(f.roots, forks.String())
}

// nodeResources is node n's record of its resources under the run's
// overrides: the cpus and memory in GB its first task, its split or its
// one main, is scheduled with, and the overrides file that gives its jobs
// what the run's gives them (Runtime.Override), which every task of the
// node hands mre stage, and whose split writes its chunks' and join's into
// the resources table they are scheduled by.
func nodeResources(rt job.Runtime, n cg.PlanNode) (string, error) {
	st := job.Stage{Fqid: n.Fqid, Using: job.Using(n.Stage), Split: n.Stage.Split}
	res := rt.Resources(st, st.Phases()[0], nil)

	over, err := rt.Override(st)
	if err != nil {
		return "", fmt.Errorf("plan: %w", err)
	}

	return fmt.Sprintf("%d %s %s", job.CPUs(res), strconv.FormatFloat(res.MemGB, 'f', -1, 64), orDash(string(over))), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func writeText(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), recordDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(text), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
