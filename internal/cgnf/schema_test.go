package cgnf

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// typedEntrySrc is an entry pipeline with an input of each type class,
// with help text, and an input the call leaves null.
const typedEntrySrc = `filetype txt;

struct Cfg(
    txt ref,
    int n,
)

stage DESC(
    in  int      n,
    in  float    f,
    in  bool     b,
    in  string   s,
    in  file     r,
    in  path     d,
    in  int[]    xs,
    in  map      m,
    in  map<txt> tm,
    in  Cfg      cfg,
    out string   desc,
    src py       "stages/desc",
)

pipeline P(
    in  int      n    "how many reads to keep",
    in  float    f,
    in  bool     b,
    in  string   s,
    in  file     r,
    in  path     d,
    in  int[]    xs,
    in  map      m,
    in  map<txt> tm,
    in  Cfg      cfg,
    out string   desc,
)
{
    call DESC(
        n   = self.n,
        f   = self.f,
        b   = self.b,
        s   = self.s,
        r   = self.r,
        d   = self.d,
        xs  = self.xs,
        m   = self.m,
        tm  = self.tm,
        cfg = self.cfg,
    )

    return (
        desc = DESC.desc,
    )
}

call P(
    n   = 1,
    f   = 0.5,
    b   = true,
    s   = null,
    r   = "/data/r.txt",
    d   = "/data",
    xs  = [1, 2],
    m   = {"k": 1},
    tm  = {"a": "/data/a.txt"},
    cfg = {
        ref: "/data/ref.txt",
        n:   3,
    },
)
`

// emitSource emits the project of the entry .mro src.
func emitSource(t *testing.T, src string) map[string]string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := Emit(filepath.Join(dir, "pipeline.mro"), Options{OutDir: out, MroPaths: []string{dir}, Version: "test"}); err != nil {
		t.Fatal(err)
	}

	return readTree(t, out)
}

// TestEmitParamsSchema: nextflow_schema.json declares each entry input at
// its root by its Martian type, described by its help text, with the
// call's value as its default unless that is null, and the project's own
// params in their groups; it declares exactly the params conf/base.config
// sets.
func TestEmitParamsSchema(t *testing.T) {
	tree := emitSource(t, typedEntrySrc)

	type group struct {
		Properties map[string]any `json:"properties"`
	}

	var doc struct {
		Defs       map[string]group `json:"$defs"`
		Properties map[string]any   `json:"properties"`
	}

	if err := json.Unmarshal([]byte(tree["nextflow_schema.json"]), &doc); err != nil {
		t.Fatal(err)
	}

	file := map[string]any{"type": "string", "format": "file-path"}
	in := func(id string, s map[string]any) map[string]any {
		s["description"] = "input " + id + " of pipeline P"

		return s
	}
	want := map[string]any{
		"n":   map[string]any{"type": "integer", "description": "how many reads to keep", "default": 1.0},
		"f":   in("f", map[string]any{"type": "number", "default": 0.5}),
		"b":   in("b", map[string]any{"type": "boolean", "default": true}),
		"s":   in("s", map[string]any{"type": "string"}),
		"r":   in("r", map[string]any{"type": "string", "format": "file-path", "default": "/data/r.txt"}),
		"d":   in("d", map[string]any{"type": "string", "format": "path", "default": "/data"}),
		"xs":  in("xs", map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "default": []any{1.0, 2.0}}),
		"m":   in("m", map[string]any{"type": "object", "default": map[string]any{"k": 1.0}}),
		"tm":  in("tm", map[string]any{"type": "object", "additionalProperties": file, "default": map[string]any{"a": "/data/a.txt"}}),
		"cfg": in("cfg", map[string]any{"type": "object", "properties": map[string]any{"ref": file, "n": map[string]any{"type": "integer"}}, "default": map[string]any{"ref": "/data/ref.txt", "n": 3.0}}),
	}

	got := maps.Clone(doc.Properties)
	delete(got, "job_resources")

	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("entry inputs (-want +got):\n%s", d)
	}

	declared := slices.Collect(maps.Keys(doc.Properties))
	for id, g := range doc.Defs {
		for p := range g.Properties {
			if (id == mro2nfGroup) != strings.HasPrefix(p, "mro2nf_") {
				t.Errorf("group %s declares %s", id, p)
			}

			declared = append(declared, p)
		}
	}

	set := regexp.MustCompile(`(?m)^params\.(\w+) = `).FindAllStringSubmatch(tree["conf/base.config"], -1)

	config := make([]string, len(set))
	for i, m := range set {
		config[i] = m[1]
	}

	if d := cmp.Diff(slices.Sorted(slices.Values(config)), slices.Sorted(slices.Values(declared))); d != "" {
		t.Errorf("schema params differ from conf/base.config's (-config +schema):\n%s", d)
	}
}

// TestEmitDeclaresEveryLaunchParam (#357): main.nf's params block declares
// exactly the params conf/base.config sets, so Nextflow fails a launch
// setting another; it types a string entry input as a String, every other
// entry input as none, an int's included, as Nextflow's Integer refuses a
// whole-number float Martian takes as an int, and each project param
// by its kind with its conf/base.config value as the default, so a boolean
// is a Boolean wherever a script reads it.
func TestEmitDeclaresEveryLaunchParam(t *testing.T) {
	tree := emitSource(t, typedEntrySrc)

	_, block, _ := strings.Cut(tree["main.nf"], "\nparams {\n")
	block, _, _ = strings.Cut(block, "\n}\n")

	got := map[string]string{}

	for l := range strings.SplitSeq(block, "\n") {
		id, decl, _ := strings.Cut(strings.TrimSpace(l), ": ")
		got[id] = decl
	}

	want := map[string]string{
		"s": "String?",
		"n": "Object?", "f": "Object?", "b": "Object?", "r": "Object?", "d": "Object?", "xs": "Object?", "m": "Object?", "tm": "Object?", "cfg": "Object?",
		"job_resources": "Map = [:]", "mro2nf_mre": "String = 'mre'", "mro2nf_mrjob": "String?", "mro2nf_adapters": "String?", "mro2nf_psid": "String = 'P'",
		"mro2nf_monitor": "Boolean = false", "mro2nf_confine": "Boolean = false",
		"mro2nf_host_inputs": "Boolean = true", "mro2nf_allow_remote_inputs": "Boolean = false", "mro2nf_host_in_place": "Boolean = true",
		"mro2nf_local_jobs": "Boolean = false", "mro2nf_s3_mount": "String?", "mro2nf_image": "String?", "mro2nf_require_image": "Boolean = false",
		"mro2nf_overrides": "String?",
	}

	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("params block (-want +got):\n%s", d)
	}

	set := regexp.MustCompile(`(?m)^params\.(\w+) = `).FindAllStringSubmatch(tree["conf/base.config"], -1)

	config := make([]string, len(set))
	for i, m := range set {
		config[i] = m[1]
	}

	if d := cmp.Diff(slices.Sorted(slices.Values(config)), slices.Sorted(maps.Keys(got))); d != "" {
		t.Errorf("params block differs from conf/base.config's params (-config +block):\n%s", d)
	}

	if strings.Contains(stageModule, "def on(") || strings.Contains(tree["main.nf"], "on(params.") {
		t.Error("a script reads a boolean param through on(), which its Boolean type makes dead")
	}
}

// TestProfileParamsHaveNoDefault: every param a -profile sets, in
// nextflow.config's profiles or the conf/container.config they include, is
// hidden and has no default, so a launch form that writes the schema's
// defaults into a params file cannot override the profile's value.
func TestProfileParamsHaveNoDefault(t *testing.T) {
	tree := emitSource(t, typedEntrySrc)

	var doc struct {
		Defs map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"$defs"`
	}

	if err := json.Unmarshal([]byte(tree["nextflow_schema.json"]), &doc); err != nil {
		t.Fatal(err)
	}

	set := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]+params\.(\w+) = `).FindAllStringSubmatch(tree["nextflow.config"], -1) {
		set[m[1]] = true
	}

	for _, m := range regexp.MustCompile(`(?m)^params\.(\w+) = `).FindAllStringSubmatch(tree["conf/container.config"], -1) {
		set[m[1]] = true
	}

	if len(set) == 0 {
		t.Fatal("no profile sets a param")
	}

	for id := range set {
		p := doc.Defs[mro2nfGroup].Properties[id]
		if p == nil {
			t.Errorf("profile param %s is not in the %s group", id, mro2nfGroup)

			continue
		}

		if p["hidden"] != true {
			t.Errorf("profile param %s is not hidden", id)
		}

		if def, ok := p["default"]; ok {
			t.Errorf("profile param %s has default %v", id, def)
		}
	}
}

// TestParamsSchemaValidatesTypes: the schema, a JSON Schema itself, admits
// the call's own values and refuses a value of another type than its
// input's.
func TestParamsSchemaValidatesTypes(t *testing.T) {
	sch := compileSchema(t, emitSource(t, typedEntrySrc)["nextflow_schema.json"])

	validate := func(params string) error {
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(params))
		if err != nil {
			t.Fatal(err)
		}

		return sch.Validate(v)
	}

	if err := validate(`{"n": 5, "f": 2, "b": false, "xs": [], "cfg": {"ref": "/x.txt", "n": 1}, "mro2nf_monitor": true}`); err != nil {
		t.Errorf("valid params refused: %v", err)
	}

	for _, params := range []string{
		`{"n": "x"}`, `{"n": 1.5}`, `{"f": "2.5"}`, `{"b": "false"}`, `{"xs": 5}`, `{"xs": [1, "a"]}`,
		`{"tm": {"a": 1}}`, `{"cfg": {"n": "x"}}`, `{"r": 5}`, `{"mro2nf_monitor": "true"}`,
	} {
		if validate(params) == nil {
			t.Errorf("params %s admitted", params)
		}
	}
}

// splitEntrySrc is an entry whose top-level map call splits its input x,
// over the literal split.
func splitEntrySrc(split string) string {
	return `stage DOUBLE(
    in  int x,
    out int y,
    src py  "stages/double",
)

pipeline DOUBLER(
    in  int x,
    out int y,
)
{
    call DOUBLE(
        x = self.x,
    )

    return (
        y = DOUBLE.y,
    )
}

map call DOUBLER(
    x = split ` + split + `,
)
`
}

// TestSplitInputSchema (#565): an input the top-level map call splits is
// declared in Martian's invocation form of a split, {"split": v}, v an
// array or a map of the input's type, with the .mro's split value so
// wrapped as its default, the HealthOmics template names that form, and the
// params block leaves it untyped.
// The schema admits the form and refuses a bare value or elements of
// another type.
func TestSplitInputSchema(t *testing.T) {
	integer := map[string]any{"type": "integer"}
	split := map[string]any{"anyOf": []any{
		map[string]any{"type": "array", "items": integer},
		map[string]any{"type": "object", "additionalProperties": integer},
	}}

	for literal, def := range map[string]any{
		"[1, 2]":           []any{1.0, 2.0},
		`{"b": 2, "a": 1}`: map[string]any{"a": 1.0, "b": 2.0},
	} {
		tree := emitSource(t, splitEntrySrc(literal))

		var doc struct {
			Properties map[string]any `json:"properties"`
		}

		if err := json.Unmarshal([]byte(tree["nextflow_schema.json"]), &doc); err != nil {
			t.Fatal(err)
		}

		want := map[string]any{
			"type": "object", "properties": map[string]any{"split": split}, "required": []any{"split"},
			"description": "input x of pipeline DOUBLER", "default": map[string]any{"split": def},
		}
		if d := cmp.Diff(want, doc.Properties["x"]); d != "" {
			t.Errorf("split %s: x (-want +got):\n%s", literal, d)
		}

		if want := `pipeline input \"x\", split per fork as {\"split\": [...]} or {\"split\": {...}}`; !strings.Contains(tree["parameter-template.json"], want) {
			t.Errorf("split %s: parameter template lacks %s:\n%s", literal, want, tree["parameter-template.json"])
		}

		if !strings.Contains(tree["main.nf"], "\n    x: Object?\n") {
			t.Errorf("split %s: the params block types the split int x, whose value is {\"split\": v}:\n%s", literal, tree["main.nf"])
		}

		checkParamsSchema(t, tree["nextflow_schema.json"])
		checkSplitValues(t, compileSchema(t, tree["nextflow_schema.json"]))
	}
}

// checkSplitValues checks sch admits x in Martian's split form only.
func checkSplitValues(t *testing.T, sch *jsonschema.Schema) {
	t.Helper()

	validate := func(params string) error {
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(params))
		if err != nil {
			t.Fatal(err)
		}

		return sch.Validate(v)
	}

	for _, params := range []string{`{"x": {"split": [4, 5]}}`, `{"x": {"split": {"r": 7, "q": 8}}}`} {
		if err := validate(params); err != nil {
			t.Errorf("params %s refused: %v", params, err)
		}
	}

	for _, params := range []string{
		`{"x": [4, 5]}`, `{"x": 5}`, `{"x": {}}`, `{"x": {"split": 5}}`, `{"x": {"split": ["a"]}}`, `{"x": {"split": {"a": 1.5}}}`,
	} {
		if validate(params) == nil {
			t.Errorf("params %s admitted", params)
		}
	}
}

// compileSchema compiles a generated nextflow_schema.json as a JSON Schema
// after checking it against nf-schema's parameter meta-schema, which
// testdata/nf-schema vendors unchanged from github.com/nextflow-io/nf-schema
// at tag 3.0.0 (commit eced945ddad56b38d3fc918e87668ce32553c863) under its
// Apache-2.0 licence. Both compile offline: the compiler loads no URL.
func compileSchema(t *testing.T, raw string) *jsonschema.Schema {
	t.Helper()

	metaRaw, err := os.ReadFile(filepath.Join("testdata", "nf-schema", "parameters_meta_schema.json"))
	if err != nil {
		t.Fatal(err)
	}

	// The meta-schema's allOf pattern leaves the $ of $defs unescaped, an
	// end-of-input anchor no $ref can match; escaped, it admits the refs
	// nf-schema's specification gives.
	const broken, escaped = `"pattern": "^#/$defs/"`, `"pattern": "^#/\\$defs/"`
	if !strings.Contains(string(metaRaw), broken) {
		t.Fatalf("the meta-schema no longer holds %s: drop its correction", broken)
	}

	meta := addResource(t, jsonschema.NewCompiler(), "file:///parameters_meta_schema.json",
		strings.NewReader(strings.Replace(string(metaRaw), broken, escaped, 1)))

	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if err := meta.Validate(doc); err != nil {
		t.Fatalf("nextflow_schema.json fails nf-schema's meta-schema: %v", err)
	}

	return addResource(t, jsonschema.NewCompiler(), "file:///nextflow_schema.json", strings.NewReader(raw))
}

func addResource(t *testing.T, c *jsonschema.Compiler, url string, r io.Reader) *jsonschema.Schema {
	t.Helper()

	doc, err := jsonschema.UnmarshalJSON(r)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}

	s, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// checkParamsSchema checks a generated nextflow_schema.json meets nf-schema's
// meta-schema and admits its own defaults, the values of the .mro's call.
func checkParamsSchema(t *testing.T, raw string) {
	t.Helper()

	if err := compileSchema(t, raw).Validate(schemaDefaults(t, raw)); err != nil {
		t.Errorf("nextflow_schema.json refuses its own defaults: %v", err)
	}
}

// schemaDefaults is the params object of every default raw declares.
func schemaDefaults(t *testing.T, raw string) map[string]any {
	t.Helper()

	type props = map[string]struct {
		Default any `json:"default"`
	}

	type group struct {
		Properties props `json:"properties"`
	}

	var doc struct {
		Defs       map[string]group `json:"$defs"`
		Properties props            `json:"properties"`
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()

	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}

	out := map[string]any{}
	add := func(ps props) {
		for id, p := range ps {
			if p.Default != nil {
				out[id] = p.Default
			}
		}
	}

	add(doc.Properties)

	for _, g := range doc.Defs {
		add(g.Properties)
	}

	return out
}
