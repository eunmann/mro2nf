package cg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"
)

// MaxValueBytes bounds the JSON of an output a post carries as a value. A
// value rides the driver's channels to every task reading it, so the driver
// holds each such output of every instance until its readers are done, and
// writes it into each reader's script, whose -resume key hashes it. Staged
// as a bundle, an output costs each reader on an object store a listing and
// a copy, about 300 ms, whatever its size. Below the bound an output holds
// a scalar, a short string or list, or a small struct, which mrp passes as
// a few bytes of JSON; above it, the driver would carry kilobytes per
// instance, which a staged bundle keeps out of its heap. The run's call,
// base64, which every task reads, is inline in each script under the same
// bound, and a file every task stages above it.
const MaxValueBytes = 4096

var errBadValue = errors.New("malformed value record")

// valueJSON is the record of one output a post carries as a value: the
// stamp of its instance and the output's JSON.
type valueJSON struct {
	instanceJSON

	Value json.RawMessage `json:"value"`
}

// valueRecord is the record of output p of the posted instance in, of value
// v, and whether the post carries it as a value: an output of a type that
// holds no file leaf, whose JSON is at most MaxValueBytes. The record is the
// output's name, a colon, and base64 of its valueJSON, so the driver selects
// it by its output without decoding it, and it crosses a shell unquoted.
func (g *Graph) valueRecord(in Instance, p *syntax.StructMember, v any) (string, bool, error) {
	files, err := g.Table().HasFiles(p.Tname)
	if err != nil {
		return "", false, fmt.Errorf("file leaves: %w", err)
	}

	if files {
		return "", false, nil
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return "", false, fmt.Errorf("encode: %w", err)
	}

	if len(raw) > MaxValueBytes {
		return "", false, nil
	}

	rec, err := json.Marshal(valueJSON{instanceJSON: in.encode(), Value: raw})
	if err != nil {
		return "", false, fmt.Errorf("encode record: %w", err)
	}

	return p.Id + ":" + base64.StdEncoding.EncodeToString(rec), true, nil
}

// readValue decodes a value record: its instance, its output and the
// output's JSON.
func (g *Graph) readValue(text string) (Instance, string, json.RawMessage, error) {
	field, enc, ok := strings.Cut(text, ":")
	if !ok || field == "" {
		return Instance{}, "", nil, fmt.Errorf("%w: %.40q", errBadValue, text)
	}

	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return Instance{}, "", nil, fmt.Errorf("value record of %s: %w", field, err)
	}

	var rec valueJSON
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Instance{}, "", nil, fmt.Errorf("value record of %s: %w", field, err)
	}

	in, err := g.instance(rec.instanceJSON)
	if err != nil {
		return Instance{}, "", nil, fmt.Errorf("value record of %s: %w", field, err)
	}

	if rec.Value == nil {
		return Instance{}, "", nil, fmt.Errorf("%w: %s of %s has no value", errBadValue, field, in.Node)
	}

	return in, field, rec.Value, nil
}
