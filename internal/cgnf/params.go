package cgnf

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// launchParam is one param a run is launched with: nextflow.config sets its
// value, nextflow_schema.json describes it, and parameter-template.json lists
// it where a HealthOmics run may set it.
type launchParam struct {
	id     string
	schema *types.Schema
	// value is the param's value in nextflow.config: nil, a string, a bool
	// or a map of strings.
	value any
	// def is the JSON value a run takes when the param is unset, nil for none.
	def json.RawMessage
	// group is the id of the param's schema group (schemaGroups), empty for
	// the schema's root.
	group string
	desc  string
	// hidden marks a param a profile sets, not the user, which the schema
	// gives no default: a launch form writing the default into a params file
	// would override the profile's value.
	hidden bool
	// omics is the param's description in the HealthOmics template, empty
	// where a HealthOmics run cannot set it.
	omics string
}

// paramGroup is a group of nextflow_schema.json. The schema's root holds
// the entry inputs and every other param whose value is an array or an
// object: nf-schema's meta-schema admits only a string, a boolean, an
// integer or a number in a group.
type paramGroup struct {
	id, title, desc string
}

const (
	outputGroup = "output_options"
	mro2nfGroup = "mro2nf_options"
)

// null is the null literal of Groovy and of JSON.
const null = "null"

func schemaGroups() []paramGroup {
	return []paramGroup{
		{id: outputGroup, title: "Output options"},
		{
			id: mro2nfGroup, title: "mro2nf options",
			desc: "The generated project's runtime: the tools every task runs, how it reads entry files and how jobs are granted resources. A profile sets those marked hidden.",
		},
	}
}

// paramBlock is a run of the project's own params nextflow.config sets
// below one comment.
type paramBlock struct {
	comment string
	params  []launchParam
}

func stringParam(id string, value any, format, desc string) launchParam {
	return launchParam{id: id, value: value, schema: &types.Schema{Type: types.JSONString, Format: format}, group: mro2nfGroup, desc: desc}
}

func boolParam(id string, value bool, desc string) launchParam {
	return launchParam{id: id, value: value, schema: &types.Schema{Type: types.JSONBoolean}, group: mro2nfGroup, desc: desc}
}

// projectParams are the params the generated project defines itself, in
// the blocks nextflow.config sets them in; psid is the call's name, the
// pipestance id a run takes by default.
func projectParams(psid string) []paramBlock {
	return slices.Concat(runParams(), toolParams(psid), inputParams())
}

func runParams() []paramBlock {
	return []paramBlock{
		{params: []launchParam{{
			id: "outdir", value: "results", schema: &types.Schema{Type: types.JSONString, Format: "directory-path"}, group: outputGroup,
			desc: "where the outputs are published: a directory or an s3:// prefix",
		}}},
		{
			comment: `// Martian 'special' scheduler-resource map (the MRO_JOBRESOURCES analog):
// a stage's special key to the clusterOptions of a grid executor.`,
			params: []launchParam{{
				id: "job_resources", value: map[string]string{},
				schema: &types.Schema{Type: types.JSONObject, AdditionalProperties: &types.Schema{Type: types.JSONString}},
				desc:   "a stage's special value to the clusterOptions of its tasks on a grid executor, as mrp's MRO_JOBRESOURCES",
			}},
		},
	}
}

func toolParams(psid string) []paramBlock {
	return []paramBlock{
		{
			comment: `// The mre every task runs, a PATH command by default: the host's on the
// local and grid profiles, the image's on the container profiles. mre runs
// py and comp stages under the mrjob beside it and py stages with the
// Martian adapters it embeds; set these to run another Martian runtime's,
// such as a pipeline bundle's own.`,
			params: []launchParam{
				stringParam("mro2nf_mre", "mre", "", "the mre the tasks run"),
				stringParam("mro2nf_mrjob", nil, "file-path", "the mrjob that runs py and comp stages; unset, the one beside mre"),
				stringParam("mro2nf_adapters", nil, "directory-path", "Martian's adapters directory, holding python/martian_shell.py; unset, the adapters mre embeds"),
			},
		},
		{
			comment: "// mrp's --monitor: mrjob enforces each job's memory and thread limits.",
			params:  []launchParam{boolParam("mro2nf_monitor", false, "mrp's --monitor: mrjob kills a job that exceeds its memory request")},
		},
		{
			comment: `// Fail a stage task whose output resolves outside its work dir instead of
// copying it in, for a run under an isolating executor such as docker.`,
			params: []launchParam{profileSet(boolParam("mro2nf_confine", false, "fail a stage task whose output resolves outside its work directory, instead of copying the file in"))},
		},
		{params: []launchParam{stringParam("mro2nf_psid", psid, "", "the pipestance ID mrp takes as its second argument; it names each job in _jobinfo")}},
	}
}

func inputParams() []paramBlock {
	return []paramBlock{
		{
			comment: `// The entry file values a run reads: a head-node path where the profile
// sees the head node's files, an s3:// URI always, and a URI of any other
// scheme only when whoever supplies the run's params is trusted to fetch it.`,
			params: []launchParam{
				profileSet(boolParam("mro2nf_host_inputs", true, "accept a head-node path as a file input")),
				boolParam("mro2nf_allow_remote_inputs", false, "accept file inputs with URI schemes other than s3://"),
			},
		},
		{
			comment: `// Every task reads a head-node path in place, the path mrp passes, on the
// profiles whose tasks run on the host's filesystem; a container profile's
// tasks see only what they stage, so there Nextflow stages it.`,
			params: []launchParam{profileSet(boolParam("mro2nf_host_in_place", true, "every task reads a head-node file input in place instead of staging it"))},
		},
		{
			comment: `// Each job's resources are granted as mrp's local job manager grants them,
// from the pool process.resourceLimits sets, its --localcores and
// --localmem, on the profiles that run every task on one host; elsewhere as
// mrp's cluster job managers grant them.`,
			params: []launchParam{profileSet(boolParam("mro2nf_local_jobs", false, "grant each job's resources as mrp's local job manager does"))},
		},
		{
			comment: `// The root under which every task's host mounts the input buckets read-only,
// <root>/<bucket>, which the awsbatch profile sets: a stage receives an
// s3:// entry input as its path there, as mrp passes the path it was given,
// so a path one stage writes out names the same file in every task, and no
// task downloads it.`,
			params: []launchParam{profileSet(stringParam("mro2nf_s3_mount", nil, "directory-path", "the directory every task's host mounts input buckets under, read-only, one directory per bucket"))},
		},
		{
			comment: `// The image every task of a container profile runs in, which those
// profiles require: one built FROM mro2nf's runtime image that holds the
// pipeline's code at the paths it was transpiled from, or the runtime image
// itself where a -c config mounts the code there.`,
			params: []launchParam{
				stringParam("mro2nf_image", nil, "", "the image every task runs in; required by the container profiles"),
				profileSet(boolParam("mro2nf_require_image", false, "fail the run unless mro2nf_image names an image")),
			},
		},
		{
			comment: `// mrp's --overrides file, over each stage's resources; PLAN reads it as
// mrp does.`,
			params: []launchParam{overridesParam()},
		},
	}
}

func overridesParam() launchParam {
	p := stringParam("mro2nf_overrides", nil, "file-path", "mrp's --overrides file, over each stage's resources")
	p.omics = "an s3:// URI of mrp's --overrides file, over each stage's resources"

	return p
}

func profileSet(p launchParam) launchParam {
	p.hidden = true

	return p
}

// entryInputs are the entry inputs as run params: each null in
// nextflow.config, keeping the value of the .mro's call, which the schema
// gives as its default.
func (p *project) entryInputs() ([]launchParam, error) {
	params, tbl, err := p.src.InParams(p.src.Invocation.Call, p.opts.MroPaths)
	if err != nil {
		return nil, fmt.Errorf("emit: entry params: %w", err)
	}

	ids := slices.Sorted(maps.Keys(p.src.Invocation.Args))
	out := make([]launchParam, len(ids))

	for i, id := range ids {
		j := slices.IndexFunc(params, func(m *syntax.StructMember) bool { return m.Id == id })
		if j < 0 {
			return nil, fmt.Errorf("emit: %w: %s", errUndeclaredInput, id)
		}

		if out[i], err = p.entryInput(params[j], tbl); err != nil {
			return nil, fmt.Errorf("emit: entry input %s: %w", id, err)
		}
	}

	return out, nil
}

// entryInput is the run param of the entry input m. One the top-level map
// call splits takes Martian's invocation form of a split, {"split": v},
// as mre plan reads it.
func (p *project) entryInput(m *syntax.StructMember, tbl *types.Table) (launchParam, error) {
	s, err := tbl.Schema(m.Tname)
	if err != nil {
		return launchParam{}, fmt.Errorf("schema: %w", err)
	}

	lp := launchParam{
		id: m.Id, schema: s, desc: cmp.Or(m.Help, fmt.Sprintf("input %s of pipeline %s", m.Id, p.src.Invocation.Call)),
		omics: fmt.Sprintf("pipeline input %q; the transpiled value when unset; a file value is an s3:// URI, or an s3:// prefix for a directory", m.Id),
	}

	def := p.src.Invocation.Args[m.Id]
	if string(def) == null {
		def = nil
	}

	if slices.Contains(p.src.Invocation.SplitArgs, m.Id) {
		lp.schema = types.Split(s)
		lp.omics = fmt.Sprintf(`pipeline input %q, split per fork as {"split": [...]} or {"split": {...}}; the transpiled value when unset; a file value is an s3:// URI, or an s3:// prefix for a directory`, m.Id)

		if def, err = splitValue(def); err != nil {
			return launchParam{}, err
		}
	}

	lp.def = def

	return lp, nil
}

// splitValue is the invocation data of a split, which carries the split's
// call and mode beside its value, as the {"split": v} a launch value gives.
func splitValue(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}

	var data struct {
		Split json.RawMessage `json:"split"`
	}

	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("split value: %w", err)
	}

	out, err := json.Marshal(map[string]json.RawMessage{"split": data.Split})
	if err != nil {
		return nil, fmt.Errorf("split value: %w", err)
	}

	return out, nil
}

// launchParams are every param of the run: the entry inputs, then the
// project's own.
func (p *project) launchParams() []launchParam {
	out := slices.Clone(p.inputs)

	for _, b := range projectParams(p.src.Invocation.Call) {
		out = append(out, b.params...)
	}

	return out
}

// groovy is v as a Groovy literal.
func groovy(v any) string {
	switch x := v.(type) {
	case string:
		return gstr(x)
	case bool:
		return strconv.FormatBool(x)
	case map[string]string:
		if len(x) == 0 {
			return "[:]"
		}

		entries := make([]string, 0, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			entries = append(entries, gstr(k)+": "+gstr(x[k]))
		}

		return "[" + strings.Join(entries, ", ") + "]"
	default:
		return null
	}
}
