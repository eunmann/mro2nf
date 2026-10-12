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
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
)

// planFlags are `mre plan`'s inputs and outputs.
type planFlags struct {
	entry, values, mropath, call, nodes, roots, counts, leaves, files, work string
	hostInputs, remoteInputs                                                bool
}

func (f *planFlags) define(fs *flag.FlagSet) {
	fs.StringVar(&f.entry, "entry", "", "the transpiled entry .mro")
	fs.StringVar(&f.values, "values", "", "JSON file of {input: value} entry values; a null keeps the transpiled one")
	fs.StringVar(&f.mropath, "mropath", "", "colon-separated include path")
	fs.StringVar(&f.call, "call", "call", "file to write the run's call into, base64 of the call as mrp's invocation generator builds it")
	fs.StringVar(&f.nodes, "nodes", "nodes", "file to write one line per witness node into: fqid digest off disabled-keys")
	fs.StringVar(&f.roots, "roots", "roots", "dir to write one file per entry-fed fork root into, a line per fork: key record index-token")
	fs.StringVar(&f.counts, "counts", "counts", "dir to write one file per entry-fed fork root into, a line per fork of its outer roots: count key")
	fs.StringVar(&f.leaves, "leaves", "leaves", "file to write one line per entry file value a task stages into: index base64-value")
	fs.StringVar(&f.files, "files", "files", "file to write one line per instance group that stages entry files into: fqid fork-key indices")
	fs.BoolVar(&f.hostInputs, "host-inputs", true, "an entry file value may name a head-node path")
	fs.BoolVar(&f.remoteInputs, "remote-inputs", false, "an entry file value may name a URI of any scheme, not only s3")
	fs.StringVar(&f.work, "work", ".", "scratch dir")
}

// runPlan starts a run: it rebuilds the invocation from the transpiled
// entry and the run's entry values as mrp builds an invocation from its
// JSON form, compiles it as mrp does, so mrp's refusals fail the run before
// any stage starts, and checks its call graph is within the witness's, and
// that every entry file value is one the run may stage. It writes the call
// every task compiles, each witness node's spec digest, disabled instances
// and entry files, and the forks of each entry-fed root.
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

	paths := filepath.SplitList(f.mropath)

	src, err := cg.ReadSource(f.entry, paths)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	inv, err := overlayInvocation(src, f.values, paths)
	if err != nil {
		return err
	}

	p, err := cg.MakePlan(src, inv, paths, f.work)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	for _, l := range p.Leaves {
		if err := cg.VetEntryLeaf(l, f.hostInputs, f.remoteInputs); err != nil {
			return fmt.Errorf("plan: %w", err)
		}
	}

	if err := f.writeEntryFiles(p); err != nil {
		return err
	}

	return f.write(p)
}

// writeEntryFiles writes the run's entry file values, each by its index,
// and per node and fork of its entry-fed roots, those its instance there
// stages.
func (f *planFlags) writeEntryFiles(p cg.Plan) error {
	var leaves, files strings.Builder

	for i, l := range p.Leaves {
		fmt.Fprintf(&leaves, "%d %s\n", i, base64.StdEncoding.EncodeToString([]byte(l.Value)))
	}

	for _, n := range p.Nodes {
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

	man, err := src.Manifest(mroPaths)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	params, err := man.Params(inv.Call, types.RoleIn)
	if err != nil {
		return nil, fmt.Errorf("entry params: %w", err)
	}

	if err := typeOverrides(over, params, man.Table()); err != nil {
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

func (f *planFlags) write(p cg.Plan) error {
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

		fmt.Fprintf(&nodes, "%s %s %s %s\n", n.Fqid, orDash(n.Digest), off, at)
	}

	if err := writeText(f.nodes, nodes.String()); err != nil {
		return err
	}

	for root, pr := range p.Roots {
		var forks, counts strings.Builder

		for _, pf := range pr.Forks {
			fmt.Fprintf(&forks, "%s %s %s\n", cg.DottedKey(pf.Key), pf.Record, pf.Token)
		}

		for _, pc := range pr.Counts {
			fmt.Fprintf(&counts, "%d %s\n", pc.N, cg.DottedKey(pc.Key))
		}

		if err := writeText(filepath.Join(f.roots, root), forks.String()); err != nil {
			return err
		}

		if err := writeText(filepath.Join(f.counts, root), counts.String()); err != nil {
			return err
		}
	}

	return nil
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
