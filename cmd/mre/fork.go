package main

import (
	"context"
	"encoding/json"
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

	forks, keys, err := bind.ResolveForks(spec, pipeArgs, callOuts)
	if err != nil {
		return fmt.Errorf("forkbind: %w", err)
	}

	for i, args := range forks {
		name := fmt.Sprintf("fork_%05d.json", i)
		if err := writeRaw(filepath.Join(*dir, name), args); err != nil {
			return err
		}
	}

	// forkkeys.json carries the map keys for a map fork (null for an array fork)
	// so merge can rebuild a keyed result.
	keysJSON := json.RawMessage("null")
	if keys != nil {
		if keysJSON, err = json.Marshal(keys); err != nil {
			return fmt.Errorf("marshal fork keys: %w", err)
		}
	}

	return writeRaw(filepath.Join(*dir, "forkkeys.json"), keysJSON)
}

// runMerge combines per-fork outputs into one map-call result: each named
// output becomes an ordered array across forks.
func runMerge(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	outs := fs.String("outs", "", "comma-separated output parameter names")
	files := fs.String("files", "", "comma-separated per-fork outs files, in order")
	keysFile := fs.String("keys-file", "", "fork keys JSON (null for an array fork)")
	outFile := fs.String("o", "", "output merged file (default stdout)")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	forkOuts, err := readFileList(splitComma(*files))
	if err != nil {
		return err
	}

	keys, err := readForkKeys(*keysFile)
	if err != nil {
		return err
	}

	merged, err := bind.Merge(splitComma(*outs), forkOuts, keys)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	return writeRaw(*outFile, merged)
}

// readForkKeys reads forkkeys.json: a JSON null (array fork) yields nil keys; a
// JSON array yields the map fork's keys.
func readForkKeys(path string) ([]string, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, nil
	}

	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("parse fork keys %s: %w", path, err)
	}

	return keys, nil
}
