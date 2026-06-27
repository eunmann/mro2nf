package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// runForkBind resolves a map call's bindings into one args file per fork,
// written as fork_NNNNN.json into -chunkdir so a lexical sort recovers order.
func runForkBind(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("forkbind", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleIn)
	specFile := fs.String("spec", "", "binding spec JSON file")
	pipeFile := fs.String("pipeargs", "", "enclosing pipeline args bundle dir")
	inputs := fs.String("inputs", "", "comma-separated callId=bundleDir pairs")
	dir := fs.String("chunkdir", ".", "directory to write per-fork args bundles")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	spec, err := readSpec(*specFile)
	if err != nil {
		return err
	}

	pipeArgs, err := readBundle(*pipeFile)
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

	// Load the type manifest once, not once per fork.
	man, err := prod.manifest()
	if err != nil {
		return err
	}

	params, tbl := man.Params(prod.callable, prod.role), man.Table()

	names := make([]string, len(forks))

	for i, args := range forks {
		name := fmt.Sprintf("fork_%05d", i)
		names[i] = name

		payload, err := rawToMap(args)
		if err != nil {
			return err
		}

		if err := shim.WriteBundle(filepath.Join(*dir, name), payload, params, tbl); err != nil {
			return fmt.Errorf("write fork bundle %s: %w", name, err)
		}
	}

	return writeForkMeta(*dir, names, keys)
}

// writeForkMeta writes the two fork sidecar files into dir:
//   - forknames.json: the fork bundle dir names, so a keyed (nested-map) workflow
//     can enumerate forks without a java.io listFiles() (which cannot list an
//     object-store s3:// work dir).
//   - forkkeys.json: the map keys for a map fork (null for an array fork), so
//     merge can rebuild a keyed result.
func writeForkMeta(dir string, names, keys []string) error {
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("marshal fork names: %w", err)
	}

	if err := writeRaw(filepath.Join(dir, "forknames.json"), namesJSON); err != nil {
		return err
	}

	keysJSON := json.RawMessage("null")
	if keys != nil {
		if keysJSON, err = json.Marshal(keys); err != nil {
			return fmt.Errorf("marshal fork keys: %w", err)
		}
	}

	return writeRaw(filepath.Join(dir, "forkkeys.json"), keysJSON)
}

// runMerge combines per-fork outputs into one map-call result: each named
// output becomes an ordered array across forks.
func runMerge(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleOut)
	outs := fs.String("outs", "", "comma-separated output parameter names")
	files := fs.String("files", "", "comma-separated per-fork output bundle dirs, in order")
	keysFile := fs.String("keys-file", "", "fork keys JSON (null for an array fork)")
	outFile := fs.String("o", "", "output merged bundle dir")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	forkOuts, err := readBundleList(splitComma(*files))
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

	// The merge adds a fork dimension to every output (an array for an array
	// fork, a keyed map for a map fork), so the file-leaf walk must descend one
	// level deeper than the callee's declared output types or it will skip the
	// per-fork files and leave dangling paths under object-store staging.
	return writeMerged(*outFile, merged, prod, keys != nil)
}

// writeMerged writes the merged map-call result as a bundle, bumping each output
// param's outer dimension to match the fork shape so nested files are staged.
func writeMerged(dir string, merged json.RawMessage, prod *producer, mapFork bool) error {
	man, err := prod.manifest()
	if err != nil {
		return err
	}

	payload, err := rawToMap(merged)
	if err != nil {
		return err
	}

	params := bumpForkDim(man.Params(prod.callable, prod.role), mapFork)
	if err := shim.WriteBundle(dir, payload, params, man.Table()); err != nil {
		return fmt.Errorf("write merged bundle %s: %w", dir, err)
	}

	return nil
}

// bumpForkDim raises each param's outer dimension by one to reflect the extra
// fork dimension the merge introduces.
func bumpForkDim(params []ir.Param, mapFork bool) []ir.Param {
	out := make([]ir.Param, len(params))
	copy(out, params)

	for i := range out {
		if mapFork {
			out[i].MapDim++
		} else {
			out[i].ArrayDim++
		}
	}

	return out
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
