package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strconv"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

var (
	// errNoEntryType reports a staged input with no matching entry-param type.
	errNoEntryType = errors.New("entry input has staged files but no type")
	// errStagedFew reports fewer staged files than the value's file leaves.
	errStagedFew = errors.New("too few staged files for entry input")
	// errStagedMany reports more staged files than the value's file leaves —
	// a sign the override value and the staged paths disagree on shape.
	errStagedMany = errors.New("more staged files than entry input file leaves")
	// errOverrideParse reports a string override that is not a valid literal of
	// its declared scalar type.
	errOverrideParse = errors.New("entry override does not parse as its declared type")
	// errOverrideShape reports a string override for an array, map or struct
	// input, which a command-line param cannot express.
	errOverrideShape = errors.New("entry override of a non-scalar type cannot be a string")

	// intLiteral is Martian's int literal without its 19-digit cap, which
	// ParseInt's range check enforces exactly.
	intLiteral = regexp.MustCompile(`^-?[0-9]+$`)
	// jsonNumber is the JSON number grammar (RFC 8259 section 6), which also
	// rules out the hex, Inf and NaN forms strconv.ParseFloat accepts.
	jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
)

const paramsFileHint = "pass the value as typed JSON in a -params-file"

// runEntryArgs builds the entry-args bundle at run time from the baked defaults
// overlaid with run-parameter overrides. This lets the entry pipeline's inputs be
// supplied at launch (a Nextflow -params-file or AWS HealthOmics run parameters)
// instead of being fixed to the .mro's top-level call args.
//
//	-base     the baked entry_args bundle (defaults; file leaves carry through)
//	-values   a JSON object {input: value} of overrides (a null value keeps the
//	          baked default for that input)
//	-fileflat a JSON file {input: [path, ...]} — for each file-bearing input the
//	          run supplied, the Nextflow-staged file paths (one per file leaf, in
//	          the canonical type-walk order). These real local paths replace the
//	          raw override values (s3:// URIs the worker cannot stat) and are
//	          marked into the bundle. JSON keeps legal filenames containing
//	          `,`/`;`/`=` intact, which a flat separator encoding cannot.
func runEntryArgs(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("entryargs", flag.ContinueOnError)
	prod := addProducer(fs, types.RoleIn)
	base := fs.String("base", "", "baked entry_args bundle dir (defaults)")
	values := fs.String("values", "", "JSON file of {input: value} run-parameter overrides")
	fileflat := fs.String("fileflat", "", "JSON file of {input: [staged file-leaf paths]}")
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

	flatMap, err := readFlatFile(*fileflat)
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

// overlayEntry merges run-parameter overrides onto the baked payload. A string
// override of a scalar int/float/bool input is first parsed into that type, and
// every float leaf is re-encoded as mrp encodes a float literal. A
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

	params, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("entry params: %w", err)
	}

	if err := typeOverrides(over, params, man.Table()); err != nil {
		return err
	}

	if err := reconstructFiles(payload, over, flatMap, params, man.Table()); err != nil {
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

// typeOverrides replaces each string override of a scalar int, float or bool
// entry input with the typed value it spells, then re-encodes every float leaf
// the way mrp encodes a float literal (types.FormatFloats). Nextflow's strict
// (v2) parser passes every command-line --param as a String, so without this
// `--b false` would reach the stage as the truthy string "false"; and a
// -params-file 2.0 would reach it as 2.0 where mrp, given `f = 2.0`, passes 2.
func typeOverrides(over map[string]any, params []ir.Param, tbl *types.Table) error {
	for _, p := range params {
		s, ok := over[p.Name].(string)
		if !ok {
			continue
		}

		v, err := parseOverride(s, p, tbl)
		if err != nil {
			return fmt.Errorf("entry input %q declared %s, got %q: %w", p.Name, p.Type, s, err)
		}

		over[p.Name] = v
	}

	maps.Copy(over, tbl.FormatFloats(params, over))

	return nil
}

// parseOverride returns s as the value of p's declared type. A string, file,
// path or user filetype input keeps s; an array, map or struct input cannot be
// spelled as a string, since parsing it as JSON would silently reinterpret a
// value the user may have meant literally.
func parseOverride(s string, p ir.Param, tbl *types.Table) (any, error) {
	if p.ArrayDim > 0 || p.MapDim > 0 || p.BaseType == "map" || tbl.IsStruct(p.BaseType) {
		return nil, fmt.Errorf("%w; %s", errOverrideShape, paramsFileHint)
	}

	var (
		v  any
		ok bool
	)

	switch p.BaseType {
	case "int":
		v, ok = parseIntLiteral(s)
	case "float":
		v, ok = parseFloatLiteral(s)
	case "bool":
		v, ok = s == "true", s == "true" || s == "false"
	default:
		return s, nil
	}

	if !ok {
		return nil, fmt.Errorf("%w; pass a valid %s literal, or %s", errOverrideParse, p.BaseType, paramsFileHint)
	}

	return v, nil
}

// parseIntLiteral parses a base-10 int64 straight from the text, so values past
// 2^53 keep their precision.
func parseIntLiteral(s string) (int64, bool) {
	if !intLiteral.MatchString(s) {
		return 0, false
	}

	i, err := strconv.ParseInt(s, 10, 64)

	return i, err == nil
}

// parseFloatLiteral checks s is a JSON-number float in float64 range. Its
// encoding is left to typeOverrides' FormatFloats pass.
func parseFloatLiteral(s string) (json.Number, bool) {
	if !jsonNumber.MatchString(s) {
		return "", false
	}

	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return "", false
	}

	return json.Number(s), true
}

// readFlatFile reads the -fileflat JSON file: an object mapping each supplied
// file-bearing input name to its staged file-leaf paths in canonical
// type-walk order. An empty path yields an empty map (no staged inputs).
func readFlatFile(path string) (map[string][]string, error) {
	out := map[string][]string{}
	if path == "" {
		return out, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fileflat: %w", err)
	}

	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse fileflat %s: %w", path, err)
	}

	return out, nil
}
