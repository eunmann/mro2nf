package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"

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
// basename for the published outs JSON.
func publishLeaf(dir string) types.Transform {
	return func(src string) (string, error) {
		if src == "" {
			return src, nil
		}

		base := filepath.Base(src)
		if err := shim.CopyTree(src, filepath.Join(dir, base)); err != nil {
			return "", fmt.Errorf("publish %s: %w", base, err)
		}

		return base, nil
	}
}
