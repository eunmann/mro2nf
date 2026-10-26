package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	// errOverrideParse reports a string override that is not a valid literal of
	// its declared scalar type.
	errOverrideParse = errors.New("entry override does not parse as its declared type")
	// errOverrideShape reports a string override for an array, map or struct
	// input, which a command-line param cannot express.
	errOverrideShape = errors.New("entry override of a non-scalar type cannot be a string")
	// errSplitShape reports an override of a split entry input that is not
	// Martian's invocation form of a split.
	errSplitShape = errors.New(`entry override of a split input must be {"split": [...]} or {"split": {...}}`)

	// intLiteral is Martian's int literal without its 19-digit cap, which
	// ParseInt's range check enforces exactly.
	intLiteral = regexp.MustCompile(`^-?[0-9]+$`)
	// jsonNumber is the JSON number grammar (RFC 8259 section 6), which also
	// rules out the hex, Inf and NaN forms strconv.ParseFloat accepts.
	jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
)

const paramsFileHint = "pass the value as typed JSON in a -params-file"

// typeOverrides replaces each string override of a scalar int, float or bool
// entry input with the typed value it spells. Nextflow's strict (v2) parser
// passes every command-line --param as a String, so without this `--b false`
// would reach the stage as the truthy string "false". A number keeps its
// text: the rebuilt call parses it as mrp parses the literal it spells
// (core.BuildCallAst), so it reaches _args as mrp encodes that literal.
func typeOverrides(over map[string]any, params []*syntax.StructMember, tbl *types.Table) error {
	for _, p := range params {
		s, ok := over[p.Id].(string)
		if !ok {
			continue
		}

		t, err := tbl.Type(p.Tname)
		if err != nil {
			return fmt.Errorf("entry input %q: %w", p.Id, err)
		}

		v, err := parseOverride(s, t)
		if err != nil {
			return fmt.Errorf("entry input %q declared %s, got %q: %w", p.Id, p.Tname.String(), s, err)
		}

		over[p.Id] = v
	}

	return nil
}

// splitOverrides checks the override of each split entry input is Martian's
// invocation form of a split (core/runtime.go convertToExp): an object whose
// split member is an array or a map, which forks the call; a null, which
// mrp's invocation cannot split, is none. The compile types its elements, as
// mrp's does.
func splitOverrides(over map[string]any, split []string) error {
	for _, id := range split {
		v, ok := over[id]
		if !ok || isSplit(v) {
			continue
		}

		got, err := json.Marshal(types.Sorted(v))
		if err != nil {
			return fmt.Errorf("encode entry value %s: %w", id, err)
		}

		return fmt.Errorf("entry input %q, got %s: %w; %s", id, got, errSplitShape, paramsFileHint)
	}

	return nil
}

func isSplit(v any) bool {
	obj, ok := types.AsObject(v)
	if !ok {
		return false
	}

	s, _ := obj.Get("split")
	if _, arr := s.([]any); arr {
		return true
	}

	_, isMap := types.AsObject(s)

	return isMap
}

// parseOverride returns s as the value of type t. A string, file, path or
// user filetype input keeps s; an array, map or struct input cannot be
// spelled as a string, since parsing it as JSON would silently reinterpret a
// value the user may have meant literally.
func parseOverride(s string, t syntax.Type) (any, error) {
	switch tt := t.(type) {
	case *syntax.UserType:
		return s, nil
	case *syntax.BuiltinType:
		return parseBuiltin(s, tt.Id)
	default:
		return nil, fmt.Errorf("%w; %s", errOverrideShape, paramsFileHint)
	}
}

// parseBuiltin returns s as the value of the builtin type id.
func parseBuiltin(s, id string) (any, error) {
	var (
		v  any
		ok bool
	)

	switch id {
	case syntax.KindMap:
		return nil, fmt.Errorf("%w; %s", errOverrideShape, paramsFileHint)
	case syntax.KindInt:
		v, ok = parseIntLiteral(s)
	case syntax.KindFloat:
		v, ok = parseFloatLiteral(s)
	case syntax.KindBool:
		v, ok = s == "true", s == "true" || s == "false"
	default:
		return s, nil
	}

	if !ok {
		return nil, fmt.Errorf("%w; pass a valid %s literal, or %s", errOverrideParse, id, paramsFileHint)
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

// parseFloatLiteral checks s is a JSON number in float64 range, and keeps
// its text.
func parseFloatLiteral(s string) (json.Number, bool) {
	if !jsonNumber.MatchString(s) {
		return "", false
	}

	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return "", false
	}

	return json.Number(s), true
}
