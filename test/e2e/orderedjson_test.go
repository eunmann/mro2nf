package e2e

// The comparators decode JSON into ordered objects: mrp passes a struct value
// through with its producer's key order, so key order is a difference the
// differential sees. Numbers keep their source token, so an int-vs-float
// change stays visible too. They are an oracle independent of
// types.DecodeJSON, the decoder under test, which folds a repeated key into
// one and ignores trailing data: these keep the one and reject the other.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// jsonMember is one member of a decoded JSON object.
type jsonMember struct {
	Key   string
	Value any
}

// jsonObject is a decoded JSON object, its members in source order.
type jsonObject []jsonMember

// errJSONShape reports JSON the ordered decoder cannot read.
var errJSONShape = errors.New("malformed JSON")

// decodeOrderedJSON decodes one JSON document into strings, json.Number, bool,
// nil, []any and jsonObject values, rejecting trailing data.
func decodeOrderedJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after JSON value", errJSONShape)
	}

	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errJSONShape, err)
	}

	switch tok {
	case json.Delim('{'):
		return decodeOrderedObject(dec)
	case json.Delim('['):
		arr := []any{}

		for dec.More() {
			v, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}

			arr = append(arr, v)
		}

		return arr, closeDelim(dec)
	default:
		return tok, nil
	}
}

func decodeOrderedObject(dec *json.Decoder) (any, error) {
	obj := jsonObject{}

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errJSONShape, err)
		}

		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: object key %v", errJSONShape, tok)
		}

		v, err := decodeOrderedValue(dec)
		if err != nil {
			return nil, err
		}

		obj = append(obj, jsonMember{Key: key, Value: v})
	}

	return obj, closeDelim(dec)
}

func closeDelim(dec *json.Decoder) error {
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%w: %w", errJSONShape, err)
	}

	return nil
}

// encodeOrderedJSON writes a decoded value back as compact JSON, objects in
// member order and strings without HTML escaping.
func encodeOrderedJSON(v any) (string, error) {
	var b strings.Builder
	if err := writeOrderedJSON(&b, v); err != nil {
		return "", err
	}

	return b.String(), nil
}

func writeOrderedJSON(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case jsonObject:
		b.WriteByte('{')

		for i, m := range x {
			if i > 0 {
				b.WriteByte(',')
			}

			if err := writeOrderedJSON(b, m.Key); err != nil {
				return err
			}

			b.WriteByte(':')

			if err := writeOrderedJSON(b, m.Value); err != nil {
				return err
			}
		}

		b.WriteByte('}')
	case []any:
		b.WriteByte('[')

		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}

			if err := writeOrderedJSON(b, e); err != nil {
				return err
			}
		}

		b.WriteByte(']')
	default:
		return writeJSONScalar(b, v)
	}

	return nil
}

func writeJSONScalar(b *strings.Builder, v any) error {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode %v: %w", v, err)
	}

	b.WriteString(strings.TrimSuffix(buf.String(), "\n"))

	return nil
}

// pairJSONStrings replaces every string value in v (not object keys) with f's
// result for it and its counterpart in other: the value at the same object
// key or array index, nil where other has none.
func pairJSONStrings(v, other any, f func(s string, other any) string) any {
	switch x := v.(type) {
	case string:
		return f(x, other)
	case jsonObject:
		o, _ := other.(jsonObject)

		for i := range x {
			var counterpart any

			if j := slices.IndexFunc(o, func(m jsonMember) bool { return m.Key == x[i].Key }); j >= 0 {
				counterpart = o[j].Value
			}

			x[i].Value = pairJSONStrings(x[i].Value, counterpart, f)
		}

		return x
	case []any:
		o, _ := other.([]any)

		for i := range x {
			var counterpart any

			if i < len(o) {
				counterpart = o[i]
			}

			x[i] = pairJSONStrings(x[i], counterpart, f)
		}

		return x
	default:
		return v
	}
}

// mapJSONStrings replaces every string value in v (not object keys) with
// f's result.
func mapJSONStrings(v any, f func(string) (string, error)) (any, error) {
	switch x := v.(type) {
	case string:
		return f(x)
	case jsonObject:
		for i := range x {
			nv, err := mapJSONStrings(x[i].Value, f)
			if err != nil {
				return nil, err
			}

			x[i].Value = nv
		}

		return x, nil
	case []any:
		for i := range x {
			nv, err := mapJSONStrings(x[i], f)
			if err != nil {
				return nil, err
			}

			x[i] = nv
		}

		return x, nil
	default:
		return v, nil
	}
}
