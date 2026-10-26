package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/google/go-cmp/cmp"
)

// fixtureDir is testdata/fixture, absolute.
func fixtureDir(t *testing.T, fixture string) string {
	t.Helper()

	fx, err := filepath.Abs(filepath.Join("..", "..", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}

	return fx
}

// plan runs mre plan on fixture with the entry values and flags,
// and returns the plan dir.
func plan(t *testing.T, fixture, values string, flags ...string) (string, error) {
	t.Helper()

	return planEntry(t, fixtureDir(t, fixture), values, flags...)
}

// planEntry runs mre plan on the pipeline.mro in dir with the entry values
// and flags, and returns the plan dir.
func planEntry(t *testing.T, dir, values string, flags ...string) (string, error) {
	t.Helper()

	work := t.TempDir()

	vf := filepath.Join(work, "values.json")
	if err := os.WriteFile(vf, []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}

	err := run(t.Context(), append([]string{
		"plan", "-entry", filepath.Join(dir, "pipeline.mro"), "-values", vf, "-mropath", dir, "-work", work, "-psid", "mrp",
		"-call", filepath.Join(work, "call"), "-nodes", filepath.Join(work, "nodes"), "-roots", filepath.Join(work, "roots"),
		"-leaves", filepath.Join(work, "leaves"), "-files", filepath.Join(work, "files"), "-pipelines", filepath.Join(work, "pipelines"),
	}, flags...))

	return work, err
}

// TestPlanFindsThePipelinesVersion (#625): PLAN finds the pipelines'
// version once for the run, as mrp does at its start (cmd/mrp/configure.go
// GetMroVersion), on the include path the stage tasks find the code on, not
// on its own copy of the .mro files, and writes it for every task's
// _jobinfo.
func TestPlanFindsThePipelinesVersion(t *testing.T) {
	code := filepath.Join(t.TempDir(), "mro")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(filepath.Dir(code), ".version"), "v9.9.9\n")

	work, err := plan(t, "diamond_min", `{}`, "-code-mropath", code)
	if err != nil {
		t.Fatal(err)
	}

	if got := readText(t, filepath.Join(work, "pipelines")); got != "v9.9.9" {
		t.Errorf("PLAN wrote the pipelines' version %q, want v9.9.9, the code's", got)
	}
}

// planCall is the run's call the plan in work wrote, decoded.
func planCall(t *testing.T, work string) string {
	t.Helper()

	call, err := base64.StdEncoding.DecodeString(readText(t, filepath.Join(work, "call")))
	if err != nil {
		t.Fatal(err)
	}

	return string(call)
}

func readText(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// rootLines are root's lines of the plan's record of every entry-fed root
// at path, less the root each leads with.
func rootLines(t *testing.T, path, root string) []string {
	t.Helper()

	var out []string

	for line := range strings.SplitSeq(readText(t, path), "\n") {
		if rest, ok := strings.CutPrefix(line, root+" "); ok {
			out = append(out, rest)
		}
	}

	return out
}

// TestPlanWritesEveryRootInOneRecord (#509): the forks of every entry-fed
// root are one record, each line led by its root, the roots in order, so
// PLAN's outputs are the same few however many roots a pipeline has
// (map_nested_gather: BY_STAGE and BY_ENTRY over the entry, BY_ENTRY.SQ
// within each BY_ENTRY fork; BY_STAGE.SQ is stage-fed).
func TestPlanWritesEveryRootInOneRecord(t *testing.T) {
	work, err := plan(t, "map_nested_gather", `{}`)
	if err != nil {
		t.Fatal(err)
	}

	var roots []string

	for line := range strings.SplitSeq(strings.TrimSuffix(readText(t, filepath.Join(work, "roots")), "\n"), "\n") {
		if root := strings.Fields(line)[0]; !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}

	if d := cmp.Diff([]string{"MNG.BY_ENTRY", "MNG.BY_ENTRY.SQ", "MNG.BY_STAGE"}, roots); d != "" {
		t.Errorf("roots of the forks record, in order (-want +plan):\n%s", d)
	}
}

// TestPlanNestedEntryForks: a run of map_pipe_nested with its sets widened
// at launch, which the witness transpile does not fix, keys the inner map
// call's forks by the outer fork and its own, as mrp forks OUTER/INNER/DBL:
// three under the first set and one under the second, and two under each
// when every set has two, a fork set of known length the witness's varies
// with the outer fork. Each DBL fork's records give it mrp's _args.
func TestPlanNestedEntryForks(t *testing.T) {
	cases := []struct {
		sets string
		want []string
	}{
		{`[[1,2,3],[4]]`, []string{"0.0", "0.1", "0.2", "1.0"}},
		{`[[1,2],[3,4]]`, []string{"0.0", "0.1", "1.0", "1.1"}},
	}

	wantArgs := []string{`{"x":1}`, `{"x":2}`, `{"x":3}`, `{"x":4}`}

	for _, tc := range cases {
		work, err := plan(t, "map_pipe_nested", `{"sets":`+tc.sets+`}`)
		if err != nil {
			t.Errorf("sets %s: %v", tc.sets, err)

			continue
		}

		lines := rootLines(t, filepath.Join(work, "roots"), "OUTER.INNER.DBL")
		keys := make([]string, len(lines))

		for i, line := range lines {
			keys[i] = strings.Fields(line)[0]
		}

		if d := cmp.Diff(tc.want, keys); d != "" {
			t.Errorf("sets %s: DBL's fork keys (-mrp +plan):\n%s", tc.sets, d)
		}

		if d := cmp.Diff(wantArgs, nestedArgs(t, work, lines)); d != "" {
			t.Errorf("sets %s: DBL's _args (-mrp +mre):\n%s", tc.sets, d)
		}

		if call := planCall(t, work); !strings.HasPrefix(call, "call OUTER(") || !strings.Contains(call, "4") {
			t.Errorf("sets %s: the call does not carry the launch values:\n%s", tc.sets, call)
		}

		if nodes := readText(t, filepath.Join(work, "nodes")); !strings.HasPrefix(nodes, "OUTER.INNER.DBL ") || strings.Count(nodes, "\n") != 1 {
			t.Errorf("sets %s: node records %q, want one for OUTER.INNER.DBL", tc.sets, nodes)
		}
	}
}

// nestedArgs runs the args step for each planned DBL fork of
// map_pipe_nested, passing the INNER fork its key names and its own record
// as the driver does, and returns each fork's _args.
func nestedArgs(t *testing.T, work string, lines []string) []string {
	t.Helper()

	inner := map[string]string{}

	for _, line := range rootLines(t, filepath.Join(work, "roots"), "OUTER.INNER") {
		f := strings.Fields(line)
		inner[f[0]] = f[1]
	}

	fx := fixtureDir(t, "map_pipe_nested")
	gf := graphFlags{entry: filepath.Join(fx, "pipeline.mro"), call: filepath.Join(work, "call"), mropath: fx}
	out := make([]string, len(lines))

	for i, line := range lines {
		f := strings.Fields(line)
		outer, _, _ := strings.Cut(f[0], ".")
		dir := filepath.Join(work, "args", strconv.Itoa(i))

		inst, args, err := argsStep(gf, argsFlags{node: "OUTER.INNER.DBL", forks: repeatedFlag{inner[outer], f[1]}}, dir)
		if err != nil || inst.Disabled {
			t.Fatalf("args of DBL fork %s: %+v %v", f[0], inst, err)
		}

		out[i] = args
	}

	return out
}

// TestPlanDigestFollowsTheNode: a node's spec digest changes with the
// entry values it reads and with no other (chain_gate: SRC reads x, USE
// reads k).
func TestPlanDigestFollowsTheNode(t *testing.T) {
	digests := func(values string) map[string]string {
		work, err := plan(t, "chain_gate", values)
		if err != nil {
			t.Fatal(err)
		}

		out := map[string]string{}

		for line := range strings.SplitSeq(strings.TrimSpace(readText(t, filepath.Join(work, "nodes"))), "\n") {
			f := strings.Fields(line)
			out[f[0]] = f[1]
		}

		return out
	}

	base, k := digests(`{}`), digests(`{"k":"7"}`)

	if base["CH.USE"] == k["CH.USE"] || base["CH.SRC"] != k["CH.SRC"] || base["CH.GATE"] != k["CH.GATE"] {
		t.Errorf("digests %v with k unset, %v with k = 7: want only USE's to change", base, k)
	}
}

// TestArgsReadsAWideCallFromItsFile: a task reads the run's call from the
// file -call names, as a call wider than the kernel's limit on one argument
// (MAX_ARG_STRLEN, 128 KiB) could not reach mre as an argument.
func TestArgsReadsAWideCallFromItsFile(t *testing.T) {
	work, err := plan(t, "chain_gate", `{"x":"5"}`)
	if err != nil {
		t.Fatal(err)
	}

	wide := planCall(t, work) + "\n# " + strings.Repeat("w", 256<<10) + "\n"
	call := writeTestFile(t, filepath.Join(work, "wide.b64"), base64.StdEncoding.EncodeToString([]byte(wide))+"\n")
	fx, args := fixtureDir(t, "chain_gate"), filepath.Join(work, "args")

	_, got, err := argsStep(graphFlags{entry: filepath.Join(fx, "pipeline.mro"), call: call, mropath: fx}, argsFlags{node: "CH.SRC"}, args)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, `"x":5`) {
		t.Errorf("SRC's args %s, want the call's x = 5", got)
	}
}

// TestPlanAndPostAgreeOnEntryGates: SRC's post names the forks it disables
// of a map call the entry forks by the ordinals the plan keys those forks
// by, so the driver's token match disables the elements mrp disables
// (split_gate_entry, its map named with unsorted keys at launch: mrp
// disables INNER/WORK/fork0 and fork1, KEYED/WORK/fork_b and fork_c).
func TestPlanAndPostAgreeOnEntryGates(t *testing.T) {
	work, err := plan(t, "split_gate_entry", `{"named":{"c":3,"a":1,"b":2}}`)
	if err != nil {
		t.Fatal(err)
	}

	fx := fixtureDir(t, "split_gate_entry")
	mro := filepath.Join(fx, "pipeline.mro")
	gf := graphFlags{entry: mro, call: filepath.Join(work, "call"), mropath: fx}
	src, gates := "SPLIT_GATE_ENTRY.SRC", filepath.Join(work, "src", "gates")

	inst, _, err := argsStep(gf, argsFlags{node: src}, filepath.Join(work, "src", "args"))
	if err != nil {
		t.Fatal(err)
	}

	outs := map[string]any{"skips": []any{true, true, false}, "flags": map[string]any{"a": false, "b": true, "c": true}}
	if _, err := postStep(gf, inst, outs, filepath.Join(work, "src"), gates); err != nil {
		t.Fatal(err)
	}

	posted := strings.Fields(readText(t, gates))

	g, err := cg.Load(mro, planCall(t, work), []string{fx})
	if err != nil {
		t.Fatal(err)
	}

	for root, want := range map[string]map[string]bool{
		"SPLIT_GATE_ENTRY.INNER": {"0": true, "1": true, "2": false},
		"SPLIT_GATE_ENTRY.KEYED": {`"a"`: false, `"b"`: true, `"c"`: true},
	} {
		gate := cg.Gate{Node: src, Roots: []string{root}}
		got := map[string]bool{}

		for _, line := range rootLines(t, filepath.Join(work, "roots"), root) {
			f := strings.Fields(line)

			el, err := g.DecodeElement(f[1])
			if err != nil {
				t.Fatal(err)
			}

			got[el.Fork[len(el.Fork)-1].Index.GoString()] = slices.Contains(posted, cg.GateToken(root+".WORK", gate, f[0]))
		}

		if d := cmp.Diff(want, got); d != "" {
			t.Errorf("%s: disabled WORK forks by element (-mrp +mre):\n%s", root, d)
		}
	}
}

// TestPlanTypesStringParams: Nextflow passes every --param as a string, so
// the plan parses one given to a scalar input as a literal of the input's
// declared type, as mrp would read the literal, and a string that is no
// such literal fails the run before any stage starts, never reaching the
// stage as a string (#246; cli_typed: --b false would otherwise be truthy).
func TestPlanTypesStringParams(t *testing.T) {
	work, err := plan(t, "cli_typed", `{"n":"5","f":"2.5","g":"1e6","b":"false"}`)
	if err != nil {
		t.Fatal(err)
	}

	call := planCall(t, work)
	for _, want := range []string{"n = 5,", "f = 2.5,", "g = 1e+06,", "b = false,"} {
		if !strings.Contains(call, want) {
			t.Errorf("the call lacks %q:\n%s", want, call)
		}
	}

	for _, values := range []string{`{"b":"True"}`, `{"n":"2.5"}`, `{"f":"x"}`} {
		if _, err := plan(t, "cli_typed", values); !errors.Is(err, errOverrideParse) {
			t.Errorf("plan with %s: %v, want errOverrideParse", values, err)
		}
	}
}

// TestPlanRefusesAsMrp: a value that is not a literal of its input's type
// fails the plan, so no stage runs, while a runtime-gated split of a
// non-empty literal plans as upstream mrp (50ad6c0) instantiates it; mrp
// before it refused it (#387).
func TestPlanRefusesAsMrp(t *testing.T) {
	if _, err := plan(t, "chain_gate", `{"x":"four"}`); !errors.Is(err, errOverrideParse) {
		t.Errorf("plan with a non-int x: %v, want errOverrideParse", err)
	}

	if _, err := plan(t, "empty_fork_gated", `{"none":[1]}`); err != nil {
		t.Errorf("plan of a runtime-gated split of a non-empty literal: %v", err)
	}
}

// splitEntryMRO is an entry whose top-level map call forks its pipeline per
// element of its split input x.
const splitEntryMRO = `stage DOUBLE(
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
    x = split [
        1,
        2,
        3,
    ],
)
`

// TestPlanForksByTheSplitValue (#565): a launch value of a split entry
// input in Martian's invocation form, {"split": v}, forks the run by v, an
// array of another length than the .mro's or a map, as mrp forks the call
// holding v as its literal. A value in no such form fails the plan, naming
// the input.
func TestPlanForksByTheSplitValue(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pipeline.mro"), splitEntryMRO)

	cases := []struct {
		values, split string
		forks         []string
	}{
		{`{"x":{"split":[4,5]}}`, "split [\n        4,\n        5,\n    ]", []string{`{"root":"DOUBLER","index":0}`, `{"root":"DOUBLER","index":1}`}},
		{`{"x":{"split":{"r":7,"q":8}}}`, "split {\n        \"q\": 8,\n        \"r\": 7,\n    }", []string{`{"root":"DOUBLER","key":"q"}`, `{"root":"DOUBLER","key":"r"}`}},
	}

	for _, tc := range cases {
		work, err := planEntry(t, dir, tc.values)
		if err != nil {
			t.Fatalf("plan with %s: %v", tc.values, err)
		}

		if call := planCall(t, work); !strings.Contains(call, "x = "+tc.split+",") {
			t.Errorf("plan with %s: the call lacks x = %s:\n%s", tc.values, tc.split, call)
		}

		var forks []string

		for _, line := range rootLines(t, filepath.Join(work, "roots"), "DOUBLER") {
			raw, err := base64.StdEncoding.DecodeString(strings.Fields(line)[1])
			if err != nil {
				t.Fatal(err)
			}

			var rec struct {
				Fork json.RawMessage `json:"fork"`
			}

			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatal(err)
			}

			forks = append(forks, strings.Trim(string(rec.Fork), "[]"))
		}

		if d := cmp.Diff(tc.forks, forks); d != "" {
			t.Errorf("plan with %s: forks (-mrp +plan):\n%s", tc.values, d)
		}
	}

	for _, values := range []string{
		`{"x":[4,5]}`, `{"x":5}`, `{"x":"5"}`, `{"x":{}}`, `{"x":{"split":5}}`, `{"x":{"split":null}}`, `{"x":{"split":"[4,5]"}}`,
	} {
		_, err := planEntry(t, dir, values)
		if !errors.Is(err, errSplitShape) || !strings.Contains(err.Error(), `entry input "x"`) {
			t.Errorf("plan with %s: %v, want errSplitShape naming x", values, err)
		}
	}
}

// TestPlanRefusesOutOfRangeValues (#567): a launch value holding an int
// outside int64 or a float beyond float64, at any depth, fails the plan
// naming the input and the literal, as mrp's parser refuses the literal
// (syntax/parsenum.go), where it panicked; int64's ends and float64's
// largest value pass as written.
func TestPlanRefusesOutOfRangeValues(t *testing.T) {
	split := t.TempDir()
	writeTestFile(t, filepath.Join(split, "pipeline.mro"), splitEntryMRO)

	cases := []struct{ dir, values, input, literal string }{
		{fixtureDir(t, "cli_typed"), `{"n":9223372036854775808}`, "n", "9223372036854775808"},
		{fixtureDir(t, "cli_typed"), `{"n":-9223372036854775809}`, "n", "-9223372036854775809"},
		{fixtureDir(t, "cli_typed"), `{"f":1E+400}`, "f", "1E+400"},
		{fixtureDir(t, "cli_typed"), `{"f":9223372036854775808}`, "f", "9223372036854775808"},
		{fixtureDir(t, "cli_typed"), `{"f":"9223372036854775808"}`, "f", "9223372036854775808"},
		{split, `{"x":{"split":[1,9223372036854775808]}}`, "x", "9223372036854775808"},
	}

	for _, tc := range cases {
		_, err := planEntry(t, tc.dir, tc.values)
		if !errors.Is(err, cg.ErrLiteralRange) {
			t.Errorf("plan with %s: %v, want %v", tc.values, err, cg.ErrLiteralRange)

			continue
		}

		if want := fmt.Sprintf("entry input %q: %v: %s", tc.input, cg.ErrLiteralRange, tc.literal); !strings.Contains(err.Error(), want) {
			t.Errorf("plan with %s: %v, want %q", tc.values, err, want)
		}
	}

	work, err := plan(t, "cli_typed", `{"n":-9223372036854775808,"f":1.7976931348623157E+308}`)
	if err != nil {
		t.Fatal(err)
	}

	call := planCall(t, work)
	for _, want := range []string{"n = -9223372036854775808,", "f = 1.7976931348623157e+308,"} {
		if !strings.Contains(call, want) {
			t.Errorf("the call lacks %q:\n%s", want, call)
		}
	}
}

// TestPlanFloatValuesAsMrp: a float input's launch value, a JSON number or
// a command-line string, compiles to the _args value mrp writes for the
// same literal (each want from an mrp run of `f = <literal>`): a whole
// number is Martian's int literal, which a float input keeps exact.
func TestPlanFloatValuesAsMrp(t *testing.T) {
	cases := []struct{ value, want string }{
		{`9007199254740993`, "9007199254740993"},
		{`"9007199254740993"`, "9007199254740993"},
		{`-9223372036854775808`, "-9223372036854775808"},
		{`1e6`, "1e+06"},
		{`"1e6"`, "1e+06"},
		{`1.5`, "1.5"},
		{`2.0`, "2"},
		{`-0.0`, "-0"},
		{`"-0.0"`, "-0"},
		{`1E+21`, "1e+21"},
	}

	dir := fixtureDir(t, "cli_typed")

	for _, tc := range cases {
		work, err := plan(t, "cli_typed", `{"f":`+tc.value+`}`)
		if err != nil {
			t.Fatalf("plan with f = %s: %v", tc.value, err)
		}

		if got := planInput(t, dir, work, "CT.REPR", "f"); got != tc.want {
			t.Errorf("f = %s reaches _args as %s, mrp's is %s", tc.value, got, tc.want)
		}
	}
}

const nestedNumbersMRO = `struct Cfg(
    int   n,
    float x,
)

stage S(
    in  float[]    fs,
    in  map        um,
    in  int        i,
    in  Cfg        cfg,
    in  map<float> fm,
    out int        y,
    src py         "stages/s",
)

pipeline P(
    in  float[]    fs,
    in  map        um,
    in  int        i,
    in  Cfg        cfg,
    in  map<float> fm,
    out int        y,
)
{
    call S(
        fs  = self.fs,
        um  = self.um,
        i   = self.i,
        cfg = self.cfg,
        fm  = self.fm,
    )

    return (
        y = S.y,
    )
}

call P(
    fs  = [],
    um  = {},
    i   = 1,
    cfg = {"n": 1, "x": 1},
    fm  = {},
)
`

// TestPlanNestedNumbersAsMrp: a number at any depth of a launch value, of
// a float, an int, an untyped map or a struct field, compiles to the _args
// value mrp writes for the same literal (each want from an mrp run of the
// call with the launch values as its literals).
func TestPlanNestedNumbersAsMrp(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pipeline.mro"), nestedNumbersMRO)

	work, err := planEntry(t, dir, `{"fs":[2.0,0.50,9007199254740993],"um":{"y":1E6,"x":{"w":0.50,"v":1},"big":9007199254740993},`+
		`"i":2.0,"cfg":{"n":4.0,"x":4.0},"fm":{"k":-0.0}}`)
	if err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{
		"fs":  `[2,0.5,9007199254740993]`,
		"um":  `{"big":9007199254740993,"x":{"v":1,"w":0.5},"y":1e+06}`,
		"i":   `2`,
		"cfg": `{"n":4,"x":4}`,
		"fm":  `{"k":-0}`,
	} {
		if got := planInput(t, dir, work, "P.S", id); got != want {
			t.Errorf("%s reaches _args as %s, mrp's is %s", id, got, want)
		}
	}
}

// planInput is the input id of the stage node as the plan in work compiles
// it over the declarations of the pipeline.mro in dir, encoded as mrp
// encodes it in _args.
func planInput(t *testing.T, dir, work, node, id string) string {
	t.Helper()

	src, err := cg.ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	g, err := cg.Compile([]byte(src.Decls+"\n\n"+planCall(t, work)), src.Name, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	st, err := g.Stage(node)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := st.ResolvedInputs()[id].Exp.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

const besideEntryMRO = `stage FOO(
    in  int n,
    out int m,
    src py  "stages/foo",
)

pipeline P(
    in  int n,
    out int m,
)
{
    call FOO(
        n = self.n,
    )

    return (
        m = FOO.m,
    )
}

call P(
    n = 1,
)
`

// TestRunFindsStageCodeBesideEntry: a task compiles the run's call over
// the entry file, so the code of a stage the entry declares is the one
// beside the entry, as mrp finds it (syntax SrcParam.FindPath), even when a
// search root holds code at the same path and the task runs in another dir.
func TestRunFindsStageCodeBesideEntry(t *testing.T) {
	proj, shared, work := t.TempDir(), t.TempDir(), t.TempDir()

	for _, d := range []string{proj, shared} {
		if err := os.MkdirAll(filepath.Join(d, "stages", "foo"), 0o755); err != nil {
			t.Fatal(err)
		}

		writeTestFile(t, filepath.Join(d, "stages", "foo", "__init__.py"), "")
	}

	entry := writeTestFile(t, filepath.Join(proj, "pipeline.mro"), besideEntryMRO)
	t.Chdir(t.TempDir())

	if err := run(t.Context(), []string{
		"plan", "-entry", entry, "-mropath", shared, "-work", work, "-psid", "mrp",
		"-call", filepath.Join(work, "call"), "-nodes", filepath.Join(work, "nodes"), "-roots", filepath.Join(work, "roots"),
	}); err != nil {
		t.Fatal(err)
	}

	g, err := (&graphFlags{entry: entry, call: filepath.Join(work, "call"), mropath: shared}).load()
	if err != nil {
		t.Fatal(err)
	}

	st, err := g.Stage("P.FOO")
	if err != nil {
		t.Fatal(err)
	}

	stage, err := job.NewStage(st, g.MroPaths())
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(proj, "stages", "foo"); stage.Code.Cmd != want {
		t.Errorf("stage code = %s, want %s beside the entry", stage.Code.Cmd, want)
	}
}

// planEntryFile runs mre plan on entry_file with reads set to value and
// the given policy flags, and returns the plan dir.
func planEntryFile(t *testing.T, value string, flags ...string) (string, error) {
	t.Helper()

	work, fx := t.TempDir(), fixtureDir(t, "entry_file")
	vf := writeTestFile(t, filepath.Join(work, "values.json"), `{"reads": "`+value+`"}`)

	err := run(t.Context(), append([]string{
		"plan", "-entry", filepath.Join(fx, "pipeline.mro"), "-values", vf, "-mropath", fx, "-work", work, "-psid", "mrp",
		"-call", filepath.Join(work, "call"), "-nodes", filepath.Join(work, "nodes"), "-roots", filepath.Join(work, "roots"),
		"-leaves", filepath.Join(work, "leaves"), "-files", filepath.Join(work, "files"), "-pipelines", filepath.Join(work, "pipelines"),
	}, flags...))

	return work, err
}

// TestPlanRefusesEntryFilesItMayNotStage: an entry file value is vetted
// before the driver hands it to file(): a glob character in a staged value
// fails on every profile, a head-node path where the profile sees no
// head-node file, one read in place where it is or holds the run's work
// dir, and a URI scheme but s3 unless remote inputs are allowed. A
// head-node path read in place, which no task stages, may hold any
// character, as under mrp.
func TestPlanRefusesEntryFilesItMayNotStage(t *testing.T) {
	cases := []struct {
		value string
		flags []string
		ok    bool
	}{
		{"/data/reads.txt", nil, true},
		{"/data/reads.txt", []string{"-host-in-place"}, true},
		{"/data/s[1].txt", []string{"-host-in-place"}, true},
		{"/data/s[1].txt", []string{"-remote-inputs"}, false},
		{"file:///data/s[1].txt", []string{"-host-in-place", "-remote-inputs"}, false},
		{"/data/reads.txt", []string{"-host-inputs=false"}, false},
		{"/data/reads.txt", []string{"-host-in-place", "-host-inputs=false"}, false},
		{"/data", []string{"-host-in-place", "-run-workdir", "/data/proj/work"}, false},
		{"/data", []string{"-run-workdir", "/data/proj/work"}, true},
		{"/data/reads.txt", []string{"-host-in-place", "-run-workdir", "/data/proj/work"}, true},
		{"s3://bucket/reads.txt", []string{"-host-inputs=false"}, true},
		{"http://host/reads.txt", nil, false},
		{"http://host/reads.txt", []string{"-remote-inputs"}, true},
	}

	for _, tc := range cases {
		_, err := planEntryFile(t, tc.value, tc.flags...)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, cg.ErrEntryFile)) {
			t.Errorf("plan with reads %s %v: %v, want ok %t", tc.value, tc.flags, err, tc.ok)
		}
	}
}

// TestArgsReadsStagedEntryFiles: the plan lists the run's entry file, a
// head-node path a container profile's tasks do not see, and the instance
// that reads it, and the args step reads the copy the task staged, not the
// value's path.
func TestArgsReadsStagedEntryFiles(t *testing.T) {
	value := filepath.Join(t.TempDir(), "reads.txt")

	work, err := planEntryFile(t, value)
	if err != nil {
		t.Fatal(err)
	}

	if want := "0 " + base64.StdEncoding.EncodeToString([]byte(value)) + "\n"; readText(t, filepath.Join(work, "leaves")) != want {
		t.Errorf("leaves %q, want %q", readText(t, filepath.Join(work, "leaves")), want)
	}

	if got := readText(t, filepath.Join(work, "files")); got != "EP.COUNT - 0\n" {
		t.Errorf("files %q, want COUNT's instance to stage the one value", got)
	}

	fx := fixtureDir(t, "entry_file")

	t.Chdir(t.TempDir())

	if err := os.MkdirAll(filepath.Join("entry", "1"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join("entry", "1", "reads.txt"), "7\n")
	writeTestFile(t, "leaves", "0\n")

	gf := graphFlags{entry: filepath.Join(fx, "pipeline.mro"), call: filepath.Join(work, "call"), mropath: fx}
	if _, _, err := argsStep(gf, argsFlags{node: "EP.COUNT", leaves: "leaves"}, "args"); err != nil {
		t.Fatal(err)
	}

	if got := readText(t, filepath.Join("args", "f", "L0000", "reads.txt")); got != "7\n" {
		t.Errorf("the args bundle's reads holds %q, want the staged copy's", got)
	}
}

// TestArgsRefusesCodeThePlanDidNotPlan: a task compiles the code where the
// run finds it, so a .mro edited since the plan, or an image built from
// other sources, compiles the node to another spec than PLAN's digest
// names, and the args step fails rather than running it.
func TestArgsRefusesCodeThePlanDidNotPlan(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	writeTestFile(t, mro, readText(t, filepath.Join(fixtureDir(t, "chain_gate"), "pipeline.mro")))

	work, err := planEntry(t, dir, `{}`)
	if err != nil {
		t.Fatal(err)
	}

	var digest string

	for line := range strings.SplitSeq(readText(t, filepath.Join(work, "nodes")), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "CH.SRC" {
			digest = f[1]
		}
	}

	args := func() error {
		_, _, err := argsStep(graphFlags{entry: mro, call: filepath.Join(work, "call"), mropath: dir}, argsFlags{node: "CH.SRC", digest: digest}, filepath.Join(t.TempDir(), "args"))

		return err
	}

	if err := args(); err != nil {
		t.Fatalf("args of the planned code: %v", err)
	}

	writeTestFile(t, mro, strings.Replace(readText(t, mro), `"stages/src"`, `"stages/other"`, 1))

	if err := args(); !errors.Is(err, cg.ErrSpecDrift) {
		t.Errorf("args of the edited code: %v, want the spec drift error", err)
	}
}
