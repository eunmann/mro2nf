package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/eunmann/mro2nf/internal/bind"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// runGate evaluates a transpiler gate (bind.GateOff) and writes it as a
// bundle whose one field, ir.GateOutput, is the disable flag of the calls it
// gates.
func runGate(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	specFile := fs.String("spec", "", "gate binding spec JSON file")
	pipeFile := fs.String("pipeargs", "", "enclosing pipeline args bundle dir")
	inputs := fs.String("inputs", "", "comma-separated callId=bundleDir pairs")
	mapMode := fs.String("mapmode", "array", "static fork kind of the split bindings: 'map' (typed map) or 'array'")
	emptyNull := fs.Bool("emptynull", false, "zero forks of an invocation-known split source turn the gate off (#99)")
	outFile := fs.String("o", "", "output gate bundle dir")

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

	nulls, err := readStaticNulls(*pipeFile, *inputs)
	if err != nil {
		return err
	}

	off, err := bind.GateOff(spec, pipeArgs, callOuts, nulls, *mapMode == ir.MapModeMap, *emptyNull)
	if err != nil {
		return fmt.Errorf("gate: %w", err)
	}

	if err := shim.WriteBundle(*outFile, map[string]any{ir.GateOutput: off}, nil, types.NewTable(nil), nil); err != nil {
		return fmt.Errorf("write gate bundle %s: %w", *outFile, err)
	}

	return nil
}
