package main

import (
	"context"
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
	source, values, mropath, out, nodes, roots, work string
}

// runPlan starts a run: it rebuilds the invocation from the transpiled
// source and the run's entry values as mrp builds an invocation from its
// JSON form, compiles it as mrp does, so mrp's refusals fail the run before
// any stage starts, and checks its call graph is within the witness's. It
// writes the invocation every task compiles, each witness node's spec
// digest and disabled instances, and the forks of each entry-fed root.
func runPlan(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)

	var f planFlags

	fs.StringVar(&f.source, "source", "", "the transpiled entry source (source.json)")
	fs.StringVar(&f.values, "values", "", "JSON file of {input: value} entry values; a null keeps the transpiled one")
	fs.StringVar(&f.mropath, "mropath", "", "colon-separated include path")
	fs.StringVar(&f.out, "o", "plan", "dir to write the invocation .mro into")
	fs.StringVar(&f.nodes, "nodes", "nodes", "file to write one line per witness node into: fqid digest off disabled-keys")
	fs.StringVar(&f.roots, "roots", "roots", "dir to write one file per entry-fed fork root into, a line per fork: key record")
	fs.StringVar(&f.work, "work", ".", "scratch dir")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	var src cg.Source
	if err := readRecordFile(f.source, &src); err != nil {
		return err
	}

	paths := filepath.SplitList(f.mropath)

	inv, err := overlayInvocation(src, f.values, paths)
	if err != nil {
		return err
	}

	p, err := cg.MakePlan(src, inv, paths, f.work)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}

	return f.write(src.Name, p)
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

func (f *planFlags) write(name string, p cg.Plan) error {
	if err := os.MkdirAll(f.out, recordDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", f.out, err)
	}

	tmp := filepath.Join(f.out, "."+name+".tmp")
	if err := os.WriteFile(tmp, []byte(p.Source), filePerm); err != nil {
		return fmt.Errorf("write invocation: %w", err)
	}

	if err := os.Rename(tmp, filepath.Join(f.out, name)); err != nil {
		return fmt.Errorf("write invocation: %w", err)
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

	for root, forks := range p.Roots {
		var b strings.Builder

		for _, pf := range forks {
			fmt.Fprintf(&b, "%s %s\n", dotted(pf.Key), pf.Record)
		}

		if err := writeText(filepath.Join(f.roots, root), b.String()); err != nil {
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

func dotted(key []int) string {
	parts := make([]string, len(key))
	for i, k := range key {
		parts[i] = strconv.Itoa(k)
	}

	return strings.Join(parts, ".")
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
