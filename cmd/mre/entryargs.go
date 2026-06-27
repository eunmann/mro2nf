package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/ir"
	"github.com/eunmann/martian-nextflow/internal/types"
)

var (
	// errBadFlatFlag reports a malformed -fileflat entry (not name=paths).
	errBadFlatFlag = errors.New("invalid -fileflat entry (want name=path[,path...])")
	// errNoEntryType reports a staged input with no matching entry-param type.
	errNoEntryType = errors.New("entry input has staged files but no type")
	// errStagedFew reports fewer staged files than the value's file leaves.
	errStagedFew = errors.New("too few staged files for entry input")
	// errStagedMany reports more staged files than the value's file leaves —
	// a sign the override value and the staged paths disagree on shape.
	errStagedMany = errors.New("more staged files than entry input file leaves")
)

// runEntryArgs builds the entry-args bundle at run time from the baked defaults
// overlaid with run-parameter overrides. This lets the entry pipeline's inputs be
// supplied at launch (a Nextflow -params-file or AWS HealthOmics run parameters)
// instead of being fixed to the .mro's top-level call args.
//
//	-base     the baked entry_args bundle (defaults; file leaves carry through)
//	-values   a JSON object {input: value} of overrides (a null value keeps the
//	          baked default for that input)
//	-fileflat ';'-separated name=p1,p2,... — for each file-bearing input the run
//	          supplied, the Nextflow-staged file paths (one per file leaf, in the
//	          canonical type-walk order). These real local paths replace the raw
//	          override values (s3:// URIs the worker cannot stat) and are marked
//	          into the bundle.
func runEntryArgs(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("entryargs", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleIn)
	base := fs.String("base", "", "baked entry_args bundle dir (defaults)")
	values := fs.String("values", "", "JSON file of {input: value} run-parameter overrides")
	fileflat := fs.String("fileflat", "", "';'-separated name=path[,path...] of staged file leaves")
	outFile := fs.String("o", "", "output bundle dir")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	payload := map[string]any{}

	if *base != "" {
		raw, err := readBundle(*base)
		if err != nil {
			return err
		}

		if payload, err = rawToMap(raw); err != nil {
			return fmt.Errorf("decode base entry args: %w", err)
		}
	}

	flatMap, err := parseFlatFlags(*fileflat)
	if err != nil {
		return err
	}

	if err := overlayEntry(payload, *values, flatMap, prod); err != nil {
		return err
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal entry args: %w", err)
	}

	if raw, err = prod.coerceInputs(raw, types.RoleIn); err != nil {
		return err
	}

	return prod.write(*outFile, raw)
}

// overlayEntry merges run-parameter overrides onto the baked payload. A
// file-bearing input the run supplied has its file leaves replaced with the
// Nextflow-staged paths (from flatMap, in canonical type-walk order); every other
// supplied (non-null) value is copied through; a null/absent value keeps the
// baked default.
func overlayEntry(payload map[string]any, valuesFile string, flatMap map[string][]string, prod *producer) error {
	if valuesFile == "" {
		return nil
	}

	raw, err := os.ReadFile(valuesFile)
	if err != nil {
		return fmt.Errorf("read values: %w", err)
	}

	over, err := rawToMap(raw)
	if err != nil {
		return fmt.Errorf("decode values: %w", err)
	}

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	if err := reconstructFiles(payload, over, flatMap, man.Params(prod.callable, prod.role), man.Table()); err != nil {
		return err
	}

	overlayValues(payload, over)

	return nil
}

// reconstructFiles, for each file-bearing input the run supplied (present in both
// over and flatMap with a non-null value), walks the override value's file leaves
// in the canonical type order and replaces each with the next staged path, then
// stores the result in payload and removes the key from over so overlayValues
// does not re-copy the raw (unstaged) value.
func reconstructFiles(payload, over map[string]any, flatMap map[string][]string, params []ir.Param, tbl *types.Table) error {
	byName := make(map[string]ir.Param, len(params))
	for _, p := range params {
		byName[p.Name] = p
	}

	for name, staged := range flatMap {
		v, ok := over[name]
		if !ok || v == nil {
			continue // unset → keep the baked default
		}

		p, ok := byName[name]
		if !ok {
			return fmt.Errorf("%w: %q", errNoEntryType, name)
		}

		i := 0
		out, err := tbl.Apply([]ir.Param{p}, map[string]any{name: v}, func(_ string) (string, error) {
			if i >= len(staged) {
				return "", fmt.Errorf("%w: %q (%d)", errStagedFew, name, len(staged))
			}

			s := staged[i]
			i++

			return s, nil
		})
		if err != nil {
			return fmt.Errorf("reconstruct entry input %q: %w", name, err)
		}

		if i < len(staged) {
			return fmt.Errorf("%w: %q (%d staged, %d consumed)", errStagedMany, name, len(staged), i)
		}

		payload[name] = out[name]
		delete(over, name)
	}

	return nil
}

// overlayValues copies every non-null entry of over into payload. A null (or
// absent) value keeps the baked default, so a run only changes the inputs it sets.
func overlayValues(payload, over map[string]any) {
	for k, v := range over {
		if v != nil {
			payload[k] = v
		}
	}
}

// parseFlatFlags parses the -fileflat "name=p1,p2;name2=p3" argument: ';'
// separates inputs, '=' splits name from its comma-separated staged paths.
func parseFlatFlags(s string) (map[string][]string, error) {
	out := map[string][]string{}
	if s == "" {
		return out, nil
	}

	for entry := range strings.SplitSeq(s, ";") {
		name, paths, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("%w: %q", errBadFlatFlag, entry)
		}

		if paths == "" {
			out[name] = nil

			continue
		}

		out[name] = strings.Split(paths, ",")
	}

	return out, nil
}
