package cg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/martian-lang/martian/martian/core"
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

// valueJSON is a value record's JSON: the record of the one item it stands
// in for, as that item's dir would hold it (itemKind), and the item's
// value. The item is an output of the instance its stamp names, a fork
// element its fork record names, or a partner element its partner record
// names; an absent partner record has no value.
type valueJSON struct {
	Instance *instanceJSON   `json:"instance,omitempty"`
	Fork     *elementJSON    `json:"fork,omitempty"`
	Partner  *partnerJSON    `json:"partner,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

// carried is the JSON of value v of type tname, and whether a post carries
// it as a value rather than as a bundle: a type that holds no file leaf,
// and JSON of at most MaxValueBytes.
func (g *Graph) carried(tname syntax.TypeId, v any) (json.RawMessage, bool, error) {
	files, err := g.Table().HasFiles(tname)
	if err != nil {
		return nil, false, fmt.Errorf("file leaves: %w", err)
	}

	if files {
		return nil, false, nil
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}

	if len(raw) > MaxValueBytes {
		return nil, false, nil
	}

	return raw, true, nil
}

// valueRecord is the value record of output p of the posted instance in,
// of value v, and whether the post carries it as a value (carried).
func (g *Graph) valueRecord(in Instance, p *syntax.StructMember, v any) (string, bool, error) {
	raw, ok, err := g.carried(p.Tname, v)
	if err != nil || !ok {
		return "", false, err
	}

	stamp := in.encode()

	rec, err := encodeValue(p.Id, valueJSON{Instance: &stamp, Value: raw})

	return rec, err == nil, err
}

// encodeValue is the value record of rec named name: the name, a colon,
// and base64 of rec's JSON, so the driver selects it by its name without
// decoding it, and it crosses a shell unquoted. An output's record is
// named by the output, and an element's by the dir it stands in for below
// its post's forks dir, <root>/<element>.
func encodeValue(name string, rec valueJSON) (string, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("encode value record of %s: %w", name, err)
	}

	return name + ":" + base64.StdEncoding.EncodeToString(raw), nil
}

// decodeValue decodes a value record: its name and its JSON, which holds
// one item's record and, but for an absent partner record, a value.
func decodeValue(text string) (string, valueJSON, error) {
	name, enc, ok := strings.Cut(text, ":")
	if !ok || name == "" {
		return "", valueJSON{}, fmt.Errorf("%w: %.40q", errBadValue, text)
	}

	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", valueJSON{}, fmt.Errorf("%w: %s: %w", errBadValue, name, err)
	}

	var rec valueJSON
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", valueJSON{}, fmt.Errorf("%w: %s: %w", errBadValue, name, err)
	}

	kinds := 0

	for _, set := range []bool{rec.Instance != nil, rec.Fork != nil, rec.Partner != nil} {
		if set {
			kinds++
		}
	}

	switch {
	case kinds != 1:
		return "", valueJSON{}, fmt.Errorf("%w: %s holds %d item records", errBadValue, name, kinds)
	case rec.Value == nil && !rec.absent():
		return "", valueJSON{}, fmt.Errorf("%w: %s has no value", errBadValue, name)
	}

	return name, rec, nil
}

// elementRecord is the record file of the fork or partner element rec
// stands in for, as its dir holds it, and the record.
func (rec valueJSON) elementRecord() (string, any) {
	if rec.Fork != nil {
		return elementFile, rec.Fork
	}

	return partnerFile, rec.Partner
}

// absent reports whether rec is a partner's absent record (partnerJSON).
func (rec valueJSON) absent() bool {
	return rec.Partner != nil && rec.Partner.Index == nil && rec.Partner.Key == nil
}

// ValueElement is the fork element a value record a post wrote stands in
// for (Post's ForkValues), and false for a record of any other item.
func (g *Graph) ValueElement(text string) (Element, bool, error) {
	name, rec, err := decodeValue(text)
	if err != nil || rec.Fork == nil {
		return Element{}, false, err
	}

	el, err := g.element(*rec.Fork)
	if err != nil {
		return Element{}, false, fmt.Errorf("value record of %s: %w", name, err)
	}

	return el, true, nil
}

// addValue indexes a value record as the item it stands in for.
func (s *stagedStore) addValue(text string) error {
	name, rec, err := decodeValue(text)
	if err != nil {
		return err
	}

	switch {
	case rec.Instance != nil:
		in, err := s.g.instance(*rec.Instance)
		if err != nil {
			return fmt.Errorf("value record of %s: %w", name, err)
		}

		k := s.addInstance(in)
		if s.outsValues[k] == nil {
			s.outsValues[k] = core.LazyArgumentMap{}
		}

		s.outsValues[k][name] = rec.Value
	case rec.Fork != nil:
		el, err := s.g.element(*rec.Fork)
		if err != nil {
			return fmt.Errorf("value record of %s: %w", name, err)
		}

		s.addElement(el, elementItem{value: rec.Value})
	default:
		return s.indexPartner(*rec.Partner, elementItem{value: rec.Value}, name)
	}

	return nil
}
