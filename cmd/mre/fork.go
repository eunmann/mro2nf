package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/eunmann/martian-nextflow/internal/bind"
)

// runForkBind resolves a map call's bindings into one args file per fork,
// written as fork_NNNNN.json into -chunkdir so a lexical sort recovers order.
func runForkBind(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("forkbind", flag.ContinueOnError)
	specFile := fs.String("spec", "", "binding spec JSON file")
	pipeFile := fs.String("pipeargs", "", "enclosing pipeline args JSON file")
	inputs := fs.String("inputs", "", "comma-separated callId=outsFile pairs")
	dir := fs.String("chunkdir", ".", "directory to write per-fork args files")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	spec, err := readSpec(*specFile)
	if err != nil {
		return err
	}

	pipeArgs, err := readFile(*pipeFile)
	if err != nil {
		return err
	}

	callOuts, err := readInputs(*inputs)
	if err != nil {
		return err
	}

	forks, err := bind.ResolveForks(spec, pipeArgs, callOuts)
	if err != nil {
		return fmt.Errorf("forkbind: %w", err)
	}

	for i, args := range forks {
		name := fmt.Sprintf("fork_%05d.json", i)
		if err := writeRaw(filepath.Join(*dir, name), args); err != nil {
			return err
		}
	}

	return nil
}

// runMerge combines per-fork outputs into one map-call result: each named
// output becomes an ordered array across forks.
func runMerge(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	outs := fs.String("outs", "", "comma-separated output parameter names")
	files := fs.String("files", "", "comma-separated per-fork outs files, in order")
	outFile := fs.String("o", "", "output merged file (default stdout)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	forkOuts, err := readFileList(splitComma(*files))
	if err != nil {
		return err
	}

	merged, err := bind.Merge(splitComma(*outs), forkOuts)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	return writeRaw(*outFile, merged)
}
