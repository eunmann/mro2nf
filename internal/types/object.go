package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Object is a decoded JSON object with its members in source order. mrp passes
// a value it does not rebuild through as its producer wrote it (raw bytes in
// core/resolve.go, StructType.FilterJson returning its input unchanged), so a
// value read, walked and written again keeps its producer's key order. A
// map[string]any is the other object form: it encodes with its keys sorted, the
// order of every object mrp builds itself (MarshalerMap, LazyArgumentMap, a
// struct literal's MapExp).
type Object []Member

// Member is one key of an Object and its value.
type Member struct {
	Key   string
	Value any
}

// errJSONToken reports a JSON token the ordered decoder cannot place.
var errJSONToken = errors.New("unexpected JSON token")

// Get returns the value of key k and whether o has it.
func (o Object) Get(k string) (any, bool) {
	if i := o.index(k); i >= 0 {
		return o[i].Value, true
	}

	return nil, false
}

// MarshalJSON encodes o with its members in order.
func (o Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteByte('{')

	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}

		key, err := json.Marshal(m.Key)
		if err != nil {
			return nil, fmt.Errorf("encode object key %q: %w", m.Key, err)
		}

		val, err := json.Marshal(m.Value)
		if err != nil {
			return nil, fmt.Errorf("encode object member %q: %w", m.Key, err)
		}

		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

func (o Object) index(k string) int {
	return slices.IndexFunc(o, func(m Member) bool { return m.Key == k })
}

// AsObject returns a JSON object value as an Object: an Object as it is, a
// map[string]any with its keys sorted. ok is false for any other value.
func AsObject(v any) (Object, bool) {
	switch t := v.(type) {
	case Object:
		return t, true
	case map[string]any:
		return sortedObject(t), true
	default:
		return nil, false
	}
}

// Sorted returns v with every Object replaced by a map[string]any, so it
// encodes with every key sorted, as mrp encodes a literal (syntax MapExp). v is
// not mutated.
func Sorted(v any) any {
	switch t := v.(type) {
	case Object:
		out := make(map[string]any, len(t))
		for _, m := range t {
			out[m.Key] = Sorted(m.Value)
		}

		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = Sorted(e)
		}

		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = Sorted(e)
		}

		return out
	default:
		return v
	}
}

// sortedObject returns m's members sorted by key.
func sortedObject(m map[string]any) Object {
	o := make(Object, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		o = append(o, Member{Key: k, Value: m[k]})
	}

	return o
}

// sortedIndices returns the indices of o's members in key order.
func sortedIndices(o Object) []int {
	idx := make([]int, len(o))
	for i := range idx {
		idx[i] = i
	}

	slices.SortFunc(idx, func(a, b int) int { return strings.Compare(o[a].Key, o[b].Key) })

	return idx
}

// asMap returns v as a map[string]any when it is an Object, so a walk handed a
// map hands back a map.
func asMap(v any) any {
	if o, ok := v.(Object); ok {
		return objectMap(o)
	}

	return v
}

func objectMap(o Object) map[string]any {
	out := make(map[string]any, len(o))
	for _, m := range o {
		out[m.Key] = m.Value
	}

	return out
}

// DecodeJSON decodes one JSON value into strings, json.Number, bool, nil, []any
// and Object values, so numbers keep their source token (42.0 stays 42.0) and
// objects their source key order. A repeated key keeps its first position and
// takes its last value, as encoding/json and Python's json do. Data after the
// value is not read, as with json.Decoder.Decode.
func DecodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}

	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}

	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}

	switch delim {
	case '{':
		return decodeObject(dec)
	case '[':
		return decodeArray(dec)
	default:
		return nil, fmt.Errorf("%w: %v", errJSONToken, delim)
	}
}

func decodeObject(dec *json.Decoder) (Object, error) {
	o := Object{}
	seen := map[string]int{}

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read object key: %w", err)
		}

		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: object key %v", errJSONToken, tok)
		}

		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}

		if i, dup := seen[key]; dup {
			o[i].Value = v

			continue
		}

		seen[key] = len(o)
		o = append(o, Member{Key: key, Value: v})
	}

	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("close object: %w", err)
	}

	return o, nil
}

func decodeArray(dec *json.Decoder) ([]any, error) {
	arr := []any{}

	for dec.More() {
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}

		arr = append(arr, v)
	}

	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("close array: %w", err)
	}

	return arr, nil
}
