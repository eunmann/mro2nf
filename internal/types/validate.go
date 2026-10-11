package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
)

// ErrInvalidOutputs reports stage outputs that fail mrp's output validation.
var ErrInvalidOutputs = errors.New("invalid stage outputs")

// maxFilenameLen is the longest typed-map key mrp accepts for a map of files.
const maxFilenameLen = 255

// ValidateOutputs checks a stage's final outputs (decoded with json.UseNumber)
// the way mrp's Fork.verifyOutput does (core/argument_map.go ValidateOutputs
// over syntax IsValidJson), which fails the stage on any violation: every
// declared output must be present, and each non-null value must match its
// declared type — a struct needs every member key (extra keys are fine), an int
// a JSON integer, a typed map of files filename-legal keys. A user filetype
// accepts any value, as mrp only raises an alarm for it.
func (t *Table) ValidateOutputs(params []ir.Param, outs map[string]any) error {
	var problems []string

	for _, p := range params {
		v, ok := outs[p.Name]
		if !ok {
			problems = append(problems, fmt.Sprintf("Missing output value '%s'", p.Name))

			continue
		}

		if err := t.validate(v, p.BaseType, p.ArrayDim, p.MapDim, p.IsFile); err != nil {
			problems = append(problems, fmt.Sprintf("Expected %s output value '%s' %v", p.Type, p.Name, err))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("%w:\n%s", ErrInvalidOutputs, strings.Join(problems, "\n"))
}

// validate checks one value against its type. An array dimension is the outer
// one when both are set: Martian folds a map's inner array dims into MapDim.
func (t *Table) validate(v any, base string, arrayDim, mapDim int, isFile bool) error {
	switch {
	case v == nil:
		return nil
	case arrayDim > 0:
		arr, ok := v.([]any)
		if !ok {
			return unparsable(v, "an array")
		}

		for i, e := range arr {
			if err := t.validate(e, base, arrayDim-1, mapDim, isFile); err != nil {
				return fmt.Errorf("element %d: %w", i, err)
			}
		}

		return nil
	case mapDim > 0:
		return t.validateMap(v, base, mapDim, isFile)
	}

	if fields, ok := t.structs[base]; ok {
		return t.validateStruct(v, fields)
	}

	return validateScalar(v, base)
}

// validateMap checks a typed map: its values carry mapDim-1 inner array dims,
// and a map of files (isFile) needs every key to be a legal unix filename.
func (t *Table) validateMap(v any, base string, mapDim int, isFile bool) error {
	m, ok := AsObject(v)
	if !ok {
		return unparsable(v, aMap)
	}

	for _, i := range sortedIndices(m) {
		k := m[i].Key
		if isFile {
			if err := legalFilename(k); err != nil {
				return fmt.Errorf("key %s: %w", k, err)
			}
		}

		if err := t.validate(m[i].Value, base, mapDim-1, 0, isFile); err != nil {
			return fmt.Errorf("key %s: %w", k, err)
		}
	}

	return nil
}

func (t *Table) validateStruct(v any, fields []ir.Param) error {
	m, ok := AsObject(v)
	if !ok {
		return unparsable(v, aMap)
	}

	var errs []error

	for _, f := range fields {
		e, ok := m.Get(f.Name)
		if !ok {
			errs = append(errs, fmt.Errorf("%w: %s", errMissingKey, f.Name))

			continue
		}

		if err := t.validate(e, f.BaseType, f.ArrayDim, f.MapDim, f.IsFile); err != nil {
			errs = append(errs, fmt.Errorf("key %s: %w", f.Name, err))
		}
	}

	return errors.Join(errs...)
}

var errMissingKey = errors.New("missing key")

// aMap is the "cannot be parsed as" noun of mrp's IsValidJson for an object.
const aMap = "a map"

// validateScalar checks a builtin-typed leaf (decoded with json.UseNumber) as
// mrp's IsValidJson does; any other base is a user filetype, which accepts
// every value.
func validateScalar(v any, base string) error {
	var (
		ok   bool
		what string
	)

	switch base {
	case typeInt:
		ok, what = isInt64(v), "an integer"
	case "float":
		ok, what = isFloat64(v), "a floating point number"
	case "string", typeFile, typePath:
		_, ok = v.(string)
		what = "a string"
	case "bool":
		_, ok = v.(bool)
		what = "a boolean value"
	case "map":
		_, ok = AsObject(v)
		what = aMap
	default:
		return nil
	}

	if ok {
		return nil
	}

	return unparsable(v, what)
}

func isInt64(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}

	_, err := strconv.ParseInt(n.String(), 10, 64)

	return err == nil
}

func isFloat64(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}

	_, err := strconv.ParseFloat(n.String(), 64)

	return err == nil
}

var errUnparsable = errors.New("cannot be parsed")

func unparsable(v any, what string) error {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = fmt.Append(nil, v)
	}

	const maxShown = 32
	if len(raw) > maxShown {
		raw = append(raw[:maxShown], "..."...)
	}

	return fmt.Errorf("value '%s' %w as %s", raw, errUnparsable, what)
}

var errIllegalFilename = errors.New("illegal filename")

// legalFilename ports Martian's IsLegalUnixFilename (syntax/compile_params.go).
func legalFilename(name string) error {
	switch {
	case len(name) > maxFilenameLen:
		return fmt.Errorf("%w: too long", errIllegalFilename)
	case name == "":
		return fmt.Errorf("%w: empty string", errIllegalFilename)
	case name == "." || name == "..":
		return fmt.Errorf("%w: reserved name", errIllegalFilename)
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("%w: '/' is not allowed in filenames", errIllegalFilename)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: null characters are not allowed in filenames", errIllegalFilename)
	}

	return nil
}
