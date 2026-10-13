package cgnf

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eunmann/mro2nf/internal/types"
)

// schemaDoc is nextflow_schema.json in nf-schema's format: each group
// below $defs, brought to the params' flat top level by allOf, and the
// ungrouped params at the root.
type schemaDoc struct {
	Schema      string                 `json:"$schema"`
	ID          string                 `json:"$id"`
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	Type        string                 `json:"type"`
	Defs        map[string]schemaGroup `json:"$defs"`
	AllOf       []schemaRef            `json:"allOf"`
	Properties  map[string]paramSchema `json:"properties"`
}

type schemaGroup struct {
	Title       string                 `json:"title"`
	Description string                 `json:"description,omitempty"`
	Type        string                 `json:"type"`
	Properties  map[string]paramSchema `json:"properties"`
}

type schemaRef struct {
	Ref string `json:"$ref"`
}

type paramSchema struct {
	*types.Schema

	Description string          `json:"description,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Hidden      bool            `json:"hidden,omitempty"`
}

// paramsSchema is nextflow_schema.json, which describes every launch param
// for documentation and launch forms. It validates nothing at launch: the
// nf-schema plugin that would must be downloaded, and PLAN's mre plan
// already fails a run on an entry value that is no value of its declared
// type, on every profile.
func (p *project) paramsSchema() (string, error) {
	call := p.src.Invocation.Call
	doc := schemaDoc{
		Schema:      "https://json-schema.org/draft/2020-12/schema",
		ID:          "urn:mro2nf:" + call,
		Title:       call,
		Description: fmt.Sprintf("The launch parameters of the transpiled pipeline %s: its inputs, each unset keeping the value of the .mro's call, and the project's own.", call),
		Type:        types.JSONObject,
		Defs:        map[string]schemaGroup{},
		Properties:  map[string]paramSchema{},
	}

	for _, g := range schemaGroups() {
		doc.Defs[g.id] = schemaGroup{Title: g.title, Description: g.desc, Type: types.JSONObject, Properties: map[string]paramSchema{}}
		doc.AllOf = append(doc.AllOf, schemaRef{Ref: "#/$defs/" + g.id})
	}

	for _, lp := range p.launchParams() {
		ps, err := lp.schemaOf()
		if err != nil {
			return "", err
		}

		if lp.group == "" {
			doc.Properties[lp.id] = ps
		} else {
			doc.Defs[lp.group].Properties[lp.id] = ps
		}
	}

	var b strings.Builder

	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("emit params schema: %w", err)
	}

	return b.String(), nil
}

// schemaOf is lp as nextflow_schema.json declares it, whose default is the
// call's value of an entry input and else the param's value in
// nextflow.config.
func (lp launchParam) schemaOf() (paramSchema, error) {
	def := lp.def
	if def == nil && lp.value != nil {
		raw, err := json.Marshal(lp.value)
		if err != nil {
			return paramSchema{}, fmt.Errorf("emit default of %s: %w", lp.id, err)
		}

		def = raw
	}

	return paramSchema{Schema: lp.schema, Description: lp.desc, Default: def, Hidden: lp.hidden}, nil
}
