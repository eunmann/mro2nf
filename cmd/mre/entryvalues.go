package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

var (
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
