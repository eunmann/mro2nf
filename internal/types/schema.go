package types

import (
	"errors"
	"fmt"

	"github.com/martian-lang/martian/martian/syntax"
)

// errNoSchema reports a type no JSON Schema describes.
var errNoSchema = errors.New("type has no JSON Schema")

// JSON Schema types.
const (
	JSONString  = "string"
	JSONObject  = "object"
	JSONBoolean = "boolean"
)

// nf-schema's path formats.
const (
	formatFile = "file-path"
	formatPath = "path"
)

// Schema is the JSON Schema of a value of a Martian type, as nf-schema's
// nextflow_schema.json declares a param.
type Schema struct {
	Type                 string             `json:"type"`
	Format               string             `json:"format,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
}

// Schema returns the JSON Schema of a value of the type id names. A file
// type is a string of format file-path; Martian's path is a string of
// format path, which nf-schema admits as a file or a directory, as Martian
// gives path the file kind file has (syntax.BuiltinType.IsFile). A map is
// an object, a typed map's members of its element type, and a struct an
// object of its members.
func (t *Table) Schema(id syntax.TypeId) (*Schema, error) {
	typ, err := t.Type(id)
	if err != nil {
		return nil, err
	}

	return t.schema(typ)
}

func (t *Table) schema(typ syntax.Type) (*Schema, error) {
	switch tt := typ.(type) {
	case *syntax.BuiltinType:
		return builtinSchema(tt)
	case *syntax.UserType:
		return &Schema{Type: JSONString, Format: formatFile}, nil
	case *syntax.ArrayType:
		items, err := t.schema(t.elemType(tt))
		if err != nil {
			return nil, err
		}

		return &Schema{Type: "array", Items: items}, nil
	case *syntax.TypedMapType:
		elem, err := t.schema(tt.Elem)
		if err != nil {
			return nil, err
		}

		return &Schema{Type: JSONObject, AdditionalProperties: elem}, nil
	case *syntax.StructType:
		props := make(map[string]*Schema, len(tt.Members))

		for _, m := range tt.Members {
			s, err := t.Schema(m.Tname)
			if err != nil {
				return nil, fmt.Errorf("member %s: %w", m.Id, err)
			}

			props[m.Id] = s
		}

		return &Schema{Type: JSONObject, Properties: props}, nil
	default:
		return nil, fmt.Errorf("%w: %s", errNoSchema, typ.String())
	}
}

func builtinSchema(b *syntax.BuiltinType) (*Schema, error) {
	switch b.Id {
	case syntax.KindInt:
		return &Schema{Type: "integer"}, nil
	case syntax.KindFloat:
		return &Schema{Type: "number"}, nil
	case syntax.KindBool:
		return &Schema{Type: JSONBoolean}, nil
	case syntax.KindString:
		return &Schema{Type: JSONString}, nil
	case syntax.KindFile:
		return &Schema{Type: JSONString, Format: formatFile}, nil
	case syntax.KindPath:
		return &Schema{Type: JSONString, Format: formatPath}, nil
	case syntax.KindMap:
		return &Schema{Type: JSONObject}, nil
	default:
		return nil, fmt.Errorf("%w: %s", errNoSchema, b.Id)
	}
}
