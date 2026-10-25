// Copyright (c) 2014-2017 10X Genomics, Inc. and Martian Authors. See LICENSE.martian.

package mrcore

import (
	"encoding/json"

	"github.com/martian-lang/martian/martian/syntax"
)

// possibleStructType returns true if the type name might refer to a struct
// type.  That is, if it is not an array, typed map, or builtin type.
func possibleStructType(tname syntax.TypeId, lookup *syntax.TypeLookup) bool {
	if tname.MapDim != 0 {
		// Maps are not structs.
		return false
	}
	if lookup == nil {
		// Neither is the builtin untyped map
		return tname.Tname != syntax.KindMap
	}
	t := lookup.Get(tname)
	if t == nil {
		return true
	}
	_, ok := t.(*syntax.StructType)
	return ok
}

// Recursively search an expression to convert MapExp to struct types where
// appropriate.  This should only get applied for expression types which are
// parsed from json, as opposed to those parsed from mro.
func fixExpressionTypes(exp syntax.Exp, tname syntax.TypeId, lookup *syntax.TypeLookup) {
	switch exp := exp.(type) {
	case *syntax.ArrayExp:
		if tname.ArrayDim > 0 {
			tname.ArrayDim--
		}
		for _, e := range exp.Value {
			fixExpressionTypes(e, tname, lookup)
		}
	case *syntax.MapExp:
		if tname.MapDim > 0 {
			tname.ArrayDim = tname.MapDim - 1
			tname.MapDim = 0
			for _, e := range exp.Value {
				fixExpressionTypes(e, tname, lookup)
			}
		} else if lookup == nil {
			if possibleStructType(tname, lookup) {
				exp.Kind = syntax.KindStruct
			}
		} else {
			t := lookup.Get(tname)
			if t != nil {
				if t, ok := t.(*syntax.StructType); ok {
					exp.Kind = syntax.KindStruct
					for _, member := range t.Members {
						fixExpressionTypes(exp.Value[member.Id], member.Tname, lookup)
					}
				}
			} else if possibleStructType(tname, lookup) {
				exp.Kind = syntax.KindStruct
			}
		}
	}
}

func convertToExp(parser *syntax.Parser, split bool, val json.Marshaler,
	tname syntax.TypeId, lookup *syntax.TypeLookup) (syntax.ValExp, error) {
	switch val := val.(type) {
	case syntax.ValExp:
		return val, nil
	case json.RawMessage:
		if split {
			var jv struct {
				Split json.RawMessage `json:"split"`
			}
			if err := json.Unmarshal(val, &jv); err != nil {
				return nil, err
			}
			exp, err := convertToExp(parser, false,
				jv.Split, tname, lookup)
			if n, ok := exp.(*syntax.NullExp); ok {
				return n, err
			}
			src, _ := exp.(syntax.MapCallSource)
			return &syntax.SplitExp{
				Value:  exp,
				Source: src,
			}, err
		}
		exp, err := parser.ParseValExp(val)
		fixExpressionTypes(exp, tname, lookup)
		return exp, err
	case LazyArgumentMap:
		res := syntax.MapExp{
			Kind:  syntax.KindMap,
			Value: make(map[string]syntax.Exp, len(val)),
		}
		if possibleStructType(tname, lookup) {
			res.Kind = syntax.KindStruct
		} else if tname.MapDim > 0 {
			tname.ArrayDim = tname.MapDim - 1
			tname.MapDim = 0
		}
		for k, v := range val {
			if e, err := convertToExp(parser, false,
				v, tname, lookup); err != nil {
				return &res, err
			} else {
				res.Value[k] = e
			}
		}
		return &res, nil
	case MarshalerMap:
		res := syntax.MapExp{
			Kind:  syntax.KindMap,
			Value: make(map[string]syntax.Exp, len(val)),
		}
		if possibleStructType(tname, lookup) {
			res.Kind = syntax.KindStruct
		} else if tname.MapDim > 0 {
			tname.ArrayDim = tname.MapDim - 1
			tname.MapDim = 0
		}
		for k, v := range val {
			if e, err := convertToExp(parser, false,
				v, tname, lookup); err != nil {
				return &res, err
			} else {
				res.Value[k] = e
			}
		}
		return &res, nil
	case marshallerArray:
		res := syntax.ArrayExp{
			Value: make([]syntax.Exp, 0, len(val)),
		}
		if tname.ArrayDim > 0 {
			tname.ArrayDim--
		}
		for _, v := range val {
			if e, err := convertToExp(parser, false,
				v, tname, lookup); err != nil {
				return &res, err
			} else {
				res.Value = append(res.Value, e)
			}
		}
		return &res, nil
	default:
		if val == nil {
			return new(syntax.NullExp), nil
		}
		// Simple types, e.g. string, boolean, number
		if b, err := val.MarshalJSON(); err != nil {
			return nil, err
		} else {
			return convertToExp(parser, split, json.RawMessage(b),
				tname, lookup)
		}
	}
}

