package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/bind"
)

// runBind resolves a call's input bindings into an _args JSON file. -spec is the
// static binding spec, -pipeargs is the enclosing pipeline's input args, and
// -inputs is a comma-separated list of callId=outsFile pairs for call refs.
func runBind(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("bind", flag.ContinueOnError)
	specFile := fs.String("spec", "", "binding spec JSON file")
	pipeFile := fs.String("pipeargs", "", "enclosing pipeline args JSON file")
	inputs := fs.String("inputs", "", "comma-separated callId=outsFile pairs")
	outFile := fs.String("o", "", "output args file (default stdout)")

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

	args, err := bind.Resolve(spec, pipeArgs, callOuts)
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	return writeRaw(*outFile, args)
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

		data, err := readFile(file)
		if err != nil {
			return nil, err
		}

		out[id] = data
	}

	return out, nil
}
