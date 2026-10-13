package main

import (
	"encoding/base64"
	"errors"
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

// plan runs mre plan on fixture with the entry values values, and returns
// the plan dir.
func plan(t *testing.T, fixture, values string) (string, error) {
	t.Helper()

	return planEntry(t, fixtureDir(t, fixture), values)
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
		"-counts", filepath.Join(work, "counts"), "-leaves", filepath.Join(work, "leaves"), "-files", filepath.Join(work, "files"),
	}, flags...))

	return work, err
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
// root are one record, and so are their counts, each line led by its root,
// the roots in order, so PLAN's outputs are the same few however many
// roots a pipeline has (map_nested_gather: BY_STAGE and BY_ENTRY over the
// entry, BY_ENTRY.SQ within each BY_ENTRY fork; BY_STAGE.SQ is stage-fed).
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

	want := "MNG.BY_ENTRY 3 \nMNG.BY_ENTRY.SQ 2 0\nMNG.BY_ENTRY.SQ 0 1\nMNG.BY_ENTRY.SQ 3 2\nMNG.BY_STAGE 3 \n"
	if d := cmp.Diff(want, readText(t, filepath.Join(work, "counts"))); d != "" {
		t.Errorf("counts record (-want +plan):\n%s", d)
	}
}

// TestPlanNestedEntryForks: a run of map_pipe_nested with its sets widened
// at launch, which the witness transpile does not fix, keys the inner map
// call's forks by the outer fork and its own, as mrp forks OUTER/INNER/DBL:
// three under the first set and one under the second, and two under each
// when every set has two, a fork set of known length the witness's varies
// with the outer fork. Each DBL fork's records give it mrp's _args, and
// the plan counts DBL's forks under each outer fork, for a gather over
// them to know its size.
func TestPlanNestedEntryForks(t *testing.T) {
	cases := []struct {
		sets   string
		want   []string
		counts []string
	}{
		{`[[1,2,3],[4]]`, []string{"0.0", "0.1", "0.2", "1.0"}, []string{"3 0", "1 1"}},
		{`[[1,2],[3,4]]`, []string{"0.0", "0.1", "1.0", "1.1"}, []string{"2 0", "2 1"}},
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

		if d := cmp.Diff(tc.counts, rootLines(t, filepath.Join(work, "counts"), "OUTER.INNER.DBL")); d != "" {
			t.Errorf("sets %s: DBL's fork counts (-want +plan):\n%s", tc.sets, d)
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

		inst, err := argsStep(gf, argsFlags{node: "OUTER.INNER.DBL", forks: repeatedFlag{inner[outer], f[1]}}, dir)
		if err != nil || inst.Disabled {
			t.Fatalf("args of DBL fork %s: %+v %v", f[0], inst, err)
		}

		out[i] = compactBundle(t, dir)
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

	if _, err := argsStep(graphFlags{entry: filepath.Join(fx, "pipeline.mro"), call: call, mropath: fx}, argsFlags{node: "CH.SRC"}, args); err != nil {
		t.Fatal(err)
	}

	if got := compactBundle(t, args); !strings.Contains(got, `"x":5`) {
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
	src, outs, gates := "SPLIT_GATE_ENTRY.SRC", filepath.Join(work, "src", "outs"), filepath.Join(work, "src", "gates")
	args := filepath.Join(work, "src", "args")

	if _, err := argsStep(gf, argsFlags{node: src}, args); err != nil {
		t.Fatal(err)
	}

	writeTestBundle(t, outs, map[string]any{"skips": []any{true, true, false}, "flags": map[string]any{"a": false, "b": true, "c": true}})

	if err := postStep(gf, postFlags{forks: filepath.Join(work, "src", "forks"), gates: gates}, args, outs); err != nil {
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

// TestPlanRefusesAsMrp: an invocation mrp refuses to instantiate fails the
// plan, so no stage runs (rt_gate_split_reject_pass: mrp refuses a split of
// a runtime-gated literal collection, #387), and a value that is not a
// literal of its input's type fails it too.
func TestPlanRefusesAsMrp(t *testing.T) {
	if _, err := plan(t, "chain_gate", `{"x":"four"}`); !errors.Is(err, errOverrideParse) {
		t.Errorf("plan with a non-int x: %v, want errOverrideParse", err)
	}

	if _, err := plan(t, "empty_fork_gated", `{"none":[1]}`); err == nil {
		t.Error("plan of a runtime-gated split of a non-empty literal succeeded; mrp refuses it")
	}
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
		"-counts", filepath.Join(work, "counts"), "-leaves", filepath.Join(work, "leaves"), "-files", filepath.Join(work, "files"),
	}, flags...))

	return work, err
}

// TestPlanRefusesEntryFilesItMayNotStage: an entry file value is vetted
// before the driver hands it to file(): a glob character fails on every
// profile, a head-node path where the profile sees no head-node file, and
// a URI scheme but s3 unless remote inputs are allowed.
func TestPlanRefusesEntryFilesItMayNotStage(t *testing.T) {
	cases := []struct {
		value string
		flags []string
		ok    bool
	}{
		{"/data/reads.txt", nil, true},
		{"/data/s[1].txt", []string{"-remote-inputs"}, false},
		{"/data/reads.txt", []string{"-host-inputs=false"}, false},
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

// TestArgsReadsStagedEntryFiles: the plan lists the run's entry file and
// the instance that reads it, and the args step reads the copy the task
// staged, not the value's path, which an isolated task cannot see.
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

	gf := graphFlags{entry: filepath.Join(fx, "pipeline.mro"), call: filepath.Join(work, "call"), mropath: fx}
	if _, err := argsStep(gf, argsFlags{node: "EP.COUNT", leaves: "0"}, "args"); err != nil {
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
		_, err := argsStep(graphFlags{entry: mro, call: filepath.Join(work, "call"), mropath: dir}, argsFlags{node: "CH.SRC", digest: digest}, filepath.Join(t.TempDir(), "args"))

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
