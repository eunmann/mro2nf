package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/types"
)

// runBind resolves a call's input bindings into an _args bundle directory. -spec
// is the static binding spec, -pipeargs is the enclosing pipeline's input
// bundle, and -inputs is a comma-separated list of callId=bundleDir pairs for
// call refs. The producer flags stage any file leaves into the output bundle.
func runBind(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("bind", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleIn)
	specFile := fs.String("spec", "", "binding spec JSON file")
	pipeFile := fs.String("pipeargs", "", "enclosing pipeline args bundle dir")
	inputs := fs.String("inputs", "", "comma-separated callId=bundleDir pairs")
	outFile := fs.String("o", "", "output args bundle dir")

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

	args, err := bind.Resolve(spec, pipeArgs, callOuts)
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	nulls, err := readStaticNulls(*pipeFile, *inputs)
	if err != nil {
		return err
	}

	if err := prod.writeBound(*outFile, args, spec); err != nil {
		return err
	}

	return writeStaticNulls(*outFile, bind.StaticNullParams(spec, nulls, callOuts))
}

// staticNullFile is the bundle sidecar listing the top-level fields mrp
// resolves to null when it builds the call graph (bind.StaticNulls): bind and
// forkbind write it for the values they bind, merge for a map call it prunes,
// each only when the list is not empty. It lives under the bundle's files dir
// because a de-bundled channel carries only data.json and the f/* items.
const staticNullFile = "f/staticnull.mro2nf.json"

// staticNullDirPerm is the mode of the files dir writeStaticNulls creates in a
// bundle without file leaves.
const staticNullDirPerm = 0o750

// readStaticNulls loads the static-null fields that the pipeline args bundle
// and each callId=bundleDir input record.
func readStaticNulls(pipeDir, inputs string) (bind.StaticNulls, error) {
	pipe, err := readNullFields(pipeDir)
	if err != nil {
		return bind.StaticNulls{}, err
	}

	nulls := bind.StaticNulls{Pipe: pipe, Calls: map[string]map[string]bool{}}

	for _, pair := range splitComma(inputs) {
		id, dir, ok := strings.Cut(pair, "=")
		if !ok {
			return bind.StaticNulls{}, fmt.Errorf("%w: %q", errBadInput, pair)
		}

		fields, err := readNullFields(dir)
		if err != nil {
			return bind.StaticNulls{}, err
		}

		nulls.Calls[id] = fields
	}

	return nulls, nil
}

// readNullFields reads a bundle's staticNullFile; a bundle without one, or no
// bundle, has no static-null fields.
func readNullFields(dir string) (map[string]bool, error) {
	if dir == "" {
		return nil, nil
	}

	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(staticNullFile)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read static nulls: %w", err)
	}

	var fields []string
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("parse static nulls %s: %w", dir, err)
	}

	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}

	return set, nil
}

// writeStaticNulls records fields as dir's statically null fields.
func writeStaticNulls(dir string, fields []string) error {
	if len(fields) == 0 {
		return nil
	}

	data, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshal static nulls: %w", err)
	}

	path := filepath.Join(dir, filepath.FromSlash(staticNullFile))
	if err := os.MkdirAll(filepath.Dir(path), staticNullDirPerm); err != nil {
		return fmt.Errorf("create static nulls dir: %w", err)
	}

	return writeRaw(path, data)
}

func readSpec(path string) (bind.Spec, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}

	spec := bind.Spec{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, fmt.Errorf("parse spec %s: %w", path, err)
		}
	}

	return spec, nil
}

func readInputs(inputs string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}

	for _, pair := range splitComma(inputs) {
		id, file, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q", errBadInput, pair)
		}

		data, err := readBundle(file)
		if err != nil {
			return nil, err
		}

		out[id] = data
	}

	return out, nil
}
