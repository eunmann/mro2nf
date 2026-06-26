package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/shim"
	"github.com/eunmann/martian-nextflow/internal/types"
)

// runPublish finalizes a pipeline's outputs. It reads the final output bundle
// (resolving every file leaf to a real path), copies each file-typed output —
// including those nested in arrays, typed maps, and structs — into -dir under
// its basename, and rewrites those paths to basenames so the published results
// are self-contained and location-independent.
func runPublish(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleOut)
	bundleDir := fs.String("bundle", "", "final output bundle dir")
	dir := fs.String("dir", ".", "directory to copy files into and write outs")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	raw, err := readBundle(*bundleDir)
	if err != nil {
		return err
	}

	outs, err := rawToMap(raw)
	if err != nil {
		return err
	}

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	published, err := man.Table().Apply(man.Params(prod.callable, prod.role), outs, publishLeaf(*dir))
	if err != nil {
		return fmt.Errorf("publish files: %w", err)
	}

	out, err := json.MarshalIndent(published, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal published outs: %w", err)
	}

	return writeRaw(filepath.Join(*dir, "pipeline_outs.json"), out)
}

// publishLeaf copies each file leaf into dir under its basename and returns that
// basename for the published outs JSON. Distinct leaves that share a basename
// (e.g. each fork of a map call emitting "result.txt") are disambiguated with a
// numeric suffix so none is silently overwritten.
func publishLeaf(dir string) types.Transform {
	seen := map[string]bool{}

	return func(src string) (string, error) {
		if src == "" {
			return src, nil
		}

		name := uniqueName(filepath.Base(src), seen)
		if err := shim.CopyTree(src, filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("publish %s: %w", name, err)
		}

		return name, nil
	}
}

// uniqueName returns base, or base with a numeric suffix before its extension if
// base is already taken, recording the chosen name in seen.
func uniqueName(base string, seen map[string]bool) string {
	if !seen[base] {
		seen[base] = true

		return base
	}

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if !seen[cand] {
			seen[cand] = true

			return cand
		}
	}
}
