package cgnf

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

func emitFixture(t *testing.T, name string) string {
	t.Helper()

	dir := fixturePath(t, name)
	out := t.TempDir()
	opts := Options{OutDir: out, MroPaths: []string{dir}, Version: "v1.2.3"}

	if err := Emit(filepath.Join(dir, "pipeline.mro"), opts); err != nil {
		t.Fatal(err)
	}

	return out
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		out[rel] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// TestEmitProject: a project is main.nf wiring, the one stage module, the
// config, the base config it includes and its container profiles' configs,
// the params schema, a copy of
// the entry's .mro files, and the HealthOmics package script, executable,
// and parameter template; it does not depend on where it is emitted, and
// holds no other file of the pipeline's, an entry file's default included
// (entry_file: input/default.txt).
func TestEmitProject(t *testing.T) {
	out := emitFixture(t, "entry_file")
	a, b := readTree(t, out), readTree(t, emitFixture(t, "entry_file"))

	if d := cmp.Diff(a, b); d != "" {
		t.Errorf("two emissions differ:\n%s", d)
	}

	entry := filepath.Join(fixturePath(t, "entry_file"), "pipeline.mro")
	want := []string{
		filepath.Join("_assets/mro", entry), "conf/base.config", "conf/container.config", "conf/healthomics.config",
		"main.nf", "nextflow.config", "nextflow_schema.json", "package.sh", "parameter-template.json", "stage.nf",
	}

	if d := cmp.Diff(want, slices.Sorted(maps.Keys(a))); d != "" {
		t.Errorf("project files (-want +got):\n%s", d)
	}

	if a["stage.nf"] != stageModule {
		t.Error("stage.nf is not the stage module")
	}

	for name, mode := range map[string]os.FileMode{"package.sh": execPerm, "main.nf": filePerm} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s mode %v %v, want %v", name, info, err, mode)
		}
	}
}

// TestEmitCopiesTheMroFiles: the project holds every .mro file the entry
// compiles from, its includes too, unchanged at its absolute path below
// _assets/mro, and none of the stage code beside them; PLAN stages the copy
// and caches on it, so a re-run of an unchanged project starts from
// PLAN's cached records.
func TestEmitCopiesTheMroFiles(t *testing.T) {
	tree := readTree(t, emitFixture(t, "include_test"))
	dir := fixturePath(t, "include_test")

	var got []string

	for name := range tree {
		if rest, ok := strings.CutPrefix(name, "_assets/mro/"); ok {
			got = append(got, "/"+rest)
		}
	}

	want := []string{filepath.Join(dir, "pipeline.mro"), filepath.Join(dir, "stages.mro")}
	if d := cmp.Diff(want, slices.Sorted(slices.Values(got))); d != "" {
		t.Fatalf("copied .mro files (-want +got):\n%s", d)
	}

	for _, path := range want {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if tree[filepath.Join("_assets/mro", path)] != string(raw) {
			t.Errorf("the copy of %s differs from it", path)
		}
	}

	main := tree["main.nf"]
	for _, want := range []string{"    path 'mro'\n", "file(\"${projectDir}/_assets/mro\")", "${code(meta, 'mro')}"} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf lacks %q:\n%s", want, main)
		}
	}

	if strings.Contains(main, "cache false") {
		t.Errorf("PLAN does not cache:\n%s", main)
	}
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// TestEmitNamesNoHostTools: every task runs the mre on PATH, and mre the
// mrjob beside it and the adapters it embeds, unless the run names its own,
// so a project names no tool path of the host that emitted it.
func TestEmitNamesNoHostTools(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "split_test"))["conf/base.config"]

	for _, want := range []string{"params.mro2nf_mre = 'mre'\n", "params.mro2nf_mrjob = null\n", "params.mro2nf_adapters = null\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("conf/base.config lacks %q:\n%s", want, cfg)
		}
	}

	for _, want := range []string{
		"if (run.adapters) {\n        cmd += \" -adapters ${shq(run.adapters)}\"",
		"if (run.mrjob) {\n        cmd += \" -mrjob ${shq(run.mrjob)}\"",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitConfinesAtLaunch: a stage task's outputs are confined to its
// work dir (mre stage -confine) only when the run sets params.mro2nf_confine.
func TestEmitConfinesAtLaunch(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "file_min"))["conf/base.config"]

	if !strings.Contains(cfg, "params.mro2nf_confine = false\n") {
		t.Errorf("conf/base.config does not leave confinement off:\n%s", cfg)
	}

	if !strings.Contains(stageModule, "if (run.confine) {\n        cmd += ' -confine'") || !strings.Contains(stageRun, "confine: params.mro2nf_confine,") {
		t.Error("stage.nf does not confine on params.mro2nf_confine")
	}
}

// TestEmitAliases: every stage node of the witness's call graph is one
// alias of the stage module, a split stage the split, main and join
// processes, and every entry-fed fork root takes its forks from PLAN's one
// record of them all (map_fork_upstream: GEN, then DBL forked on GEN's
// map, reads none).
func TestEmitAliases(t *testing.T) {
	main := byNode(t, "split_test")

	if !strings.Contains(main, "include { STAGE_SPLIT as N<SUM_SQUARES>_SP ; STAGE_MAIN as N<SUM_SQUARES>_MN ; STAGE_JOIN as N<SUM_SQUARES>_JN } from './stage.nf'") {
		t.Errorf("split_test's split stage is not the split triad:\n%s", main)
	}

	up := byNode(t, "map_fork_upstream")

	for _, want := range []string{
		"include { STAGE as N<GEN> } from './stage.nf'",
		"include { STAGE as N<DBL> } from './stage.nf'",
		"i<DBL> = FORK(channel.of([[], [], [], []]), ELEMENTS(o<GEN>_forks, 'MFU.DBL', [], true), [], false)",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("map_fork_upstream's main.nf lacks %q:\n%s", want, up)
		}
	}

	if strings.Contains(up, entryForks) {
		t.Errorf("map_fork_upstream has no entry-fed root, yet reads PLAN's forks:\n%s", up)
	}

	nested := wiringNF(readTree(t, emitFixture(t, "map_pipe_nested")))
	for _, want := range []string{
		"entry_forks = ENTRY_FORKS(PLAN.out.roots)",
		"ENTRY_ROOT(entry_forks, 'OUTER.INNER')",
		"ENTRY_ROOT(entry_forks, 'OUTER.INNER.DBL')",
	} {
		if !strings.Contains(nested, want) {
			t.Errorf("map_pipe_nested's wiring lacks %q:\n%s", want, nested)
		}
	}
}

// TestEmitGPURequest: every task's GPU request follows the special it is
// scheduled with, as its clusterOptions do, in one config rule and no
// per-alias one (TestGPURequestFollowsSpecial runs it).
func TestEmitGPURequest(t *testing.T) {
	tree := readTree(t, emitFixture(t, "gpu_stage"))

	if !strings.Contains(tree["conf/base.config"], "    accelerator = { def s = meta.special ?: ''; ") {
		t.Errorf("conf/base.config lacks the accelerator rule:\n%s", tree["conf/base.config"])
	}

	for _, f := range []string{"nextflow.config", "conf/base.config"} {
		if strings.Contains(tree[f], "withName:") {
			t.Errorf("%s names a process:\n%s", f, tree[f])
		}
	}

	if n := strings.Count(stageModule, "clusterOptions { params.job_resources[meta.special] ?: '' }"); n != 4 {
		t.Errorf("%d of the 4 stage processes look their special up in job_resources", n)
	}
}

// TestEmitOrdersGathers: every list a task stages from several tasks is in
// an order fixed by the run, not by completion, as that order is part of
// the task's -resume key: a join's chunk outs by chunk name (split_test), a
// gather by the upstream instance's key (fork_mid: SUMALL over SCALE's
// forks), and OUTS's inputs by node and key.
func TestEmitOrdersGathers(t *testing.T) {
	split := byNode(t, "split_test")
	gather := byNode(t, "fork_mid")

	for _, c := range []struct{ fixture, main, want string }{
		{"split_test", split, "j<SUM_SQUARES> = JOIN(x<SUM_SQUARES>, N<SUM_SQUARES>_MN.out.chunk)\n    N<SUM_SQUARES>_JN(j<SUM_SQUARES>)"},
		{"fork_mid", gather, "u<SUMALL> = READ(b<SUMALL>.run, GATHER(b<SUMALL>.run, o<SCALE>_outs, [], [], ['scaled']))\n    n<SUMALL> = INPUT(u<SUMALL>, [null])"},
		{"split_test", split, "    outs_pub = channel.empty().mix(PUBLISHED(o<SUM_SQUARES>_outs, 0, ['sum']))\n    outs_in = outs_pub.toList().map { xs -> ordered(xs) }"},
	} {
		if !strings.Contains(c.main, c.want) {
			t.Errorf("%s's main.nf lacks %q:\n%s", c.fixture, c.want, c.main)
		}
	}

	for _, want := range []string{
		"groupTuple(remainder: true).map { gk, os -> [gk.getGroupTarget(), chunkOuts(gk, os)] }",
		"return os.sort(false) { a, b -> a[0].name <=> b[0].name }",
		".collect { e -> [ku, e] }] }).groupTuple().map { c, dss ->\n        [c, ordered(dss.findAll { x -> x != null }.collectMany { x -> x })]",
		"outs.flatMap { k, ds -> selected(ds, fields).collect { e -> [[node] + k, e] } }",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitCarriesValueOutputs (#536): a posting task emits, beside its outs
// bundles, the value records of its outputs that hold no file, which the
// driver carries as one list of outs items per instance, selects and orders
// by output like the bundles, and splits apart from them for each task: it
// stages the bundles and writes the records into a file for mre.
func TestEmitCarriesValueOutputs(t *testing.T) {
	main := byNode(t, "diamond_min")

	for _, want := range []string{
		"o<GEN>_outs = POSTED(N<GEN>.out.posted, b<GEN>.skip, r<GEN>, 1)",
		"outs_files = outs_in.map { xs -> skeletons(bundlesOf(xs)) }",
		", outs_in.map { xs -> recordsOf(xs) }, ",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("diamond_min's main.nf lacks %q:\n%s", want, main)
		}
	}

	for _, want := range []string{
		"'-fields outs -field-values outs_values -field-refs outs_refs -forks forks -fork-values fork_values -fork-refs fork_refs -gates gates'",
		"posted.join(sources).map { v -> [v[0], referred(v[at], v[at + 2], v[-1] + ownOuts(v[1])) + v[at + 1].tokenize(' ')] }",
		"ds.collectEntries { d -> [('outs/' + d.name): d] }",
		"ds.findAll { e -> itemName(e) in fields }",
		"keyOrder(a[0], b[0]) ?: itemName(a[1]) <=> itemName(b[1])",
		"return e instanceof Path ? [e.parent.name, e.name] : e.substring(0, e.indexOf(':')).tokenize('/')",
		"cmd += ' -values values'",
		"cat > values <<'MRO2NF_VALUES'",
		"-in in -refs refs${values ? ' -values values' : ''}",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}

	if n := strings.Count(stageModule, postedOutput); n != 2 {
		t.Errorf("%d processes emit value records, want STAGE's and STAGE_JOIN's", n)
	}
}

// postedOutput is the one output of a posting process: its outs dirs,
// value records and refs, its fork dirs, value records and refs, and its
// gates.
const postedOutput = "tuple val(key), path('outs/*', type: 'dir', arity: '0..*'), eval('cat outs_values'), eval('cat outs_refs'), " +
	"path('forks/*/*', type: 'dir', arity: '0..*'), eval('cat fork_values'), eval('cat fork_refs'), eval('cat gates'), emit: posted"

// TestEmitPostsOneOutput (#559): every output a process declares costs
// Nextflow's DAG an edge, consumed or not, and most nodes' forks and
// gates, and some nodes' outs, have no reader, which cost CellRanger 602
// edges; so a posting process emits its outs, forks and gates as one
// tuple, which POSTED, GATES and DONE read (diamond_min: GEN's outs;
// zip_runtime: TWO's forks; split_gate_entry: SRC's gates;
// preflight_order: TOP_CHECK's DONE).
func TestEmitPostsOneOutput(t *testing.T) {
	stages, _, _ := strings.Cut(stageModule, "\nprocess OUTS {")
	if n := strings.Count(stages, "emit: "); n != 4 {
		t.Errorf("stage.nf's processes declare %d outputs, want one of each of STAGE, STAGE_SPLIT, STAGE_MAIN and STAGE_JOIN", n)
	}

	for fixture, want := range map[string]string{
		"diamond_min":      "o<GEN>_outs = POSTED(N<GEN>.out.posted, b<GEN>.skip, r<GEN>, 1)",
		"zip_runtime":      "o<TWO>_forks = POSTED(N<TWO>.out.posted, b<TWO>.skip, r<TWO>, 4)",
		"split_gate_entry": "o<SRC>_gates = GATES(N<SRC>.out.posted, b<SRC>.skip)",
		"preflight_order":  "d<TOP_CHECK> = DONE(N<TOP_CHECK>.out.posted)",
	} {
		if main := byNode(t, fixture); !strings.Contains(main, want) {
			t.Errorf("%s's wiring lacks %q:\n%s", fixture, want, main)
		}
	}
}

// TestEmitCarriesValueElements: a posting task emits, beside its fork and
// partner element dirs, the value records of the elements that hold no
// file, which the driver carries as one list of items per instance, picks
// by root (ELEMENTS) and by partner and index token (ZIP) like the dirs,
// and splits apart from them for each task, as it does outs items. On
// cellranger_shaped each of 9 instances otherwise staged a bundle of one
// integer.
func TestEmitCarriesValueElements(t *testing.T) {
	main := byNode(t, "zip_runtime")

	for _, want := range []string{
		"o<TWO>_forks = POSTED(N<TWO>.out.posted, b<TWO>.skip, r<TWO>, 4)",
		"ELEMENTS(o<TWO>_forks, 'ZIP_RUNTIME.LONG', [], false)",
		"ZIP(i<LONG>, o<THREE>_forks, [], 'ZIP_RUNTIME.LONG@ZIP_RUNTIME.THREE.xs', 0, 'none')",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("zip_runtime's main.nf lacks %q:\n%s", want, main)
		}
	}

	for _, want := range []string{
		"ds.findAll { e -> itemParent(e) == root }",
		"def name = itemName(e)",
		"ds.findAll { e -> itemParent(e) == partner }.collectEntries { e -> [(itemName(e)): e] }",
		"def items = v[3] + readItems(v, fields)\n    return [v[4], v[1], v[2], recordsOf(items), bundlesOf(items), refsOf(items), ids, entries, callStaged(v[4])]",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}

	if n := strings.Count(stageModule, postedOutput); n != 2 {
		t.Errorf("%d processes emit element value records, want STAGE's and STAGE_JOIN's", n)
	}

	if strings.Contains(stageModule, "e.parent.name ==") {
		t.Error("stage.nf picks elements by their dir alone, which misses value records")
	}
}

// TestEmitLocatesOutsInputs: OUTS stages the skeleton of each outs bundle,
// and of each bundle holding a file they name by reference, never the
// bundle (#610), and gets beside each where its producing task's work dir
// keeps the bundle, which names a file the pipeline outs leave unpublished
// (#457) and the source each published file is published from, and passes
// both to mre outs.
func TestEmitLocatesOutsInputs(t *testing.T) {
	main := readTree(t, emitFixture(t, "nested_path_unpublished"))["main.nf"]

	for _, want := range []string{
		"outs_files = outs_in.map { xs -> skeletons(bundlesOf(xs)) }",
		"outs_refs = outs_in.map { xs -> skeletons(refBundles(xs)) }",
		", outs_files, outs_files.map { ss -> bundleLocations(ss) }, outs_refs, outs_refs.map { ss -> bundleLocations(ss) }, ",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf lacks %q:\n%s", want, main)
		}
	}

	for _, want := range []string{
		"path ups, stageAs: 'in/u*', arity: '0..*'\n    val locs\n    path refs, stageAs: 'refs/?', arity: '0..*'\n    val refLocs",
		"${locations(ups, locs, refLocs)}", " -locations locations", "return ss.collect { s -> s.parent.toUriString() }",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("OUTS lacks %q", want)
		}
	}

	if !strings.Contains(stageModule, "d.resolve('"+shim.SkeletonFile+"')") {
		t.Errorf("skeletons names no %s", shim.SkeletonFile)
	}
}

// TestEmitPublishesThroughWorkflowOutputs (#580): OUTS publishes nothing
// itself; the entry workflow publishes its outs JSON, and each entry of
// the outs tree its manifest lists from where the task that wrote it keeps
// it to its path in the tree (#610), OUTS's own external/ copy for a file
// no stage task wrote, as copies, below Nextflow's outputDir, which only
// the healthomics profile sets, to the HealthOmics pubdir; no param names
// it. A -preview run, which runs no task, publishes nothing, as an enabled
// output would hang it (TestLaunchEdges).
func TestEmitPublishesThroughWorkflowOutputs(t *testing.T) {
	tree := readTree(t, emitFixture(t, "dir_out"))

	checkContains(t, tree, "main.nf",
		"workflow {\n    main:\n    if (",
		"\n    publish:\n    pipeline_outs = OUTS.out.json\n    results = OUTS.out.manifest.flatMap { m -> published(m) }\n}\n",
		"\noutput {\n    pipeline_outs {\n        enabled !workflow.preview\n        mode 'copy'\n    }\n"+
			"    results {\n        enabled !workflow.preview\n        mode 'copy'\n        path { e -> e[0] >> e[1] }\n    }\n}\n",
	)
	checkContains(t, tree, "stage.nf",
		"    path 'pipeline_outs.json', emit: json\n    path 'publish', emit: manifest\n",
		"new groovy.json.JsonSlurper().parseText(p.resolve('publish.json').text).collect { e -> [e.local ? p.resolve(e.source) : file(e.source, glob: false), e.path] }",
	)
	checkContains(t, tree, "conf/healthomics.config", "\noutputDir = '/mnt/workflow/pubdir'\n")

	if strings.Contains(stageModule, "publishDir") {
		t.Error("stage.nf publishes through publishDir")
	}

	for name, src := range tree {
		if name != "conf/healthomics.config" && strings.Contains(src, "outputDir =") {
			t.Errorf("%s sets outputDir:\n%s", name, src)
		}

		if strings.Contains(src, "outdir") {
			t.Errorf("%s names an outdir param:\n%s", name, src)
		}
	}
}

// TestEmitCarriesTheCall: one rule, CALL's, chooses how every task gets
// the run's call: inline, a value its script writes into call.b64, when
// its base64 is at most cg.MaxValueBytes, as on AWS Batch a staged file
// costs each task a download (~300 ms); or else the one file the driver
// writes once per run, which each task stages as call.b64, as with N entry
// forks an inline call grew every task's script, and the -resume key
// Nextflow hashes from it, by O(N) bytes (#551).
func TestEmitCarriesTheCall(t *testing.T) {
	main := readTree(t, emitFixture(t, "split_test"))["main.nf"]

	for _, want := range []string{
		"shared = CALL(PLAN.out.call, " + strconv.Itoa(cg.MaxValueBytes) + ").map { c -> [entry: ",
		"OUTS(shared, shared.map { m -> callStaged(m) }, outs_files, ",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf lacks %q:\n%s", want, main)
		}
	}

	for _, want := range []string{
		"inline: v.size() <= bound\n        staged: true\n",
		"out = c.inline.mix(c.staged.collectFile(name: 'call.b64')).first()",
		"return meta.call instanceof Path ? '' : \"cat > call.b64 <<'MRO2NF_CALL'\\n${meta.call}\\nMRO2NF_CALL\"",
		"return meta.call instanceof Path ? [meta.call] : []",
		"\"${code(meta, root)} -call call.b64\"",
		"refsOf(items), ids, entries, callStaged(v[4])]",
		"c.name, c, callStaged(m)] }",
		"ids, ef, cs, j, os.collect { o -> o[0] }, callStaged(m)]",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}

	for _, proc := range []string{"STAGE", "STAGE_SPLIT", "STAGE_MAIN", "STAGE_JOIN", "OUTS"} {
		_, body, _ := strings.Cut(stageModule, "\nprocess "+proc+" {")
		body, _, _ = strings.Cut(body, "\n}\n")
		input, script, _ := strings.Cut(body, "output:\n")
		_, script, _ = strings.Cut(script, "script:\n")

		if !strings.Contains(input, "path(cf, stageAs: 'call.b64', arity: '0..*')") && !strings.Contains(input, "path cf, stageAs: 'call.b64', arity: '0..*'\n") {
			t.Errorf("%s does not stage the call file CALL staged:\n%s", proc, input)
		}

		if at := strings.Index(script, "${callScript(meta)}"); at < 0 || at != strings.Index(script, "${") {
			t.Errorf("%s does not write an inline call before it runs mre:\n%s", proc, script)
		}
	}
}

// TestEmitRunsOneMreCallPerTask: each stage process runs its phase in one
// mre stage call, and the stage module runs mre only there and in OUTS.
func TestEmitRunsOneMreCallPerTask(t *testing.T) {
	for proc, phase := range map[string]string{"STAGE": "main", "STAGE_SPLIT": "split", "STAGE_MAIN": "main", "STAGE_JOIN": "join"} {
		_, body, _ := strings.Cut(stageModule, "\nprocess "+proc+" {")
		_, script, _ := strings.Cut(body, "script:\n")
		script, _, _ = strings.Cut(script, "\n}\n")

		if strings.Count(script, "${mreStage(") != 1 || !strings.Contains(script, "${mreStage(task, meta, '"+phase+"', ") {
			t.Errorf("%s does not run its %s phase in one mre stage call:\n%s", proc, phase, script)
		}
	}

	if n := strings.Count(stageModule, "${shq(run.mre)} ") + strings.Count(stageModule, "${shq(meta.run.mre)} "); n != 2 {
		t.Errorf("stage.nf runs mre at %d places, want mreStage's and OUTS's", n)
	}
}

// TestEmitStagesChunkApart: a chunk's main stages the split's chunk dir
// below chunk/, apart from the job's metadata dir of the chunk's name, so
// the job writes nothing into the split's outputs, which would change their
// -resume key (split_test).
func TestEmitStagesChunkApart(t *testing.T) {
	if !strings.Contains(stageModule, ", val(name), path(chunk, stageAs: 'chunk/*'), ") {
		t.Error("STAGE_MAIN does not stage its chunk below chunk/")
	}
}

// TestEmitSplitTasksBuildTheirArgs: a split's chunk mains and join stage
// the split's inputs, as STAGE does, and build the instance's args from
// them, so no task outputs args and none carries a copy of a file they
// name per fork (#507; split_test wires one instance channel into all
// three).
func TestEmitSplitTasksBuildTheirArgs(t *testing.T) {
	inputs := "tuple val(meta), val(key), val(forks), val(values), path(ups, stageAs: 'in/u?', arity: '0..*'), path(refs, stageAs: 'refs/?/f/*', arity: '0..*'), val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')"

	for _, proc := range []string{"STAGE", "STAGE_SPLIT", "STAGE_MAIN", "STAGE_JOIN"} {
		_, body, _ := strings.Cut(stageModule, "\nprocess "+proc+" {")
		body, _, _ = strings.Cut(body, "\n}\n")
		_, output, _ := strings.Cut(body, "output:\n")

		for _, want := range []string{inputs, "${valuesFile(values)}\n    mkdir -p in refs\n", "${args(meta, forks, values, leaves)} -args args "} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q:\n%s", proc, want, body)
			}
		}

		if strings.Contains(output, "'args'") {
			t.Errorf("%s outputs its args:\n%s", proc, output)
		}
	}

	main := byNode(t, "split_test")

	for _, want := range []string{
		"s<SUM_SQUARES> = INPUT(b<SUM_SQUARES>.run, [])",
		"N<SUM_SQUARES>_SP(s<SUM_SQUARES>)",
		sumSquaresTriad,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("split_test's main.nf lacks %q:\n%s", want, main)
		}
	}
}

// sumSquaresTriad is the wiring of split node SUM_SQUARES's split, its chunks'
// mains and its join, past the split's call: the chunks and the join read
// the split's outputs joined to its input once (#559), which cost
// CellRanger 6 edges of Nextflow's DAG per split stage when each joined
// them anew.
const sumSquaresTriad = "    x<SUM_SQUARES> = SPLITS(N<SUM_SQUARES>_SP.out.split, s<SUM_SQUARES>)\n" +
	"    N<SUM_SQUARES>_MN(CHUNKS(x<SUM_SQUARES>))\n" +
	"    j<SUM_SQUARES> = JOIN(x<SUM_SQUARES>, N<SUM_SQUARES>_MN.out.chunk)\n" +
	"    N<SUM_SQUARES>_JN(j<SUM_SQUARES>)\n"

// TestEmitSizesChunksAndJoin: each chunk's main and the join are scheduled
// with the resources their jobs get, the split's resources table, a value
// the driver parses no record for (join_resources: the join requests 2
// threads and 3 GB; TestWriteResources writes the table).
func TestEmitSizesChunksAndJoin(t *testing.T) {
	main := byNode(t, "join_resources")

	if !strings.Contains(main, sumSquaresTriad) {
		t.Errorf("join_resources's main.nf lacks %q:\n%s", sumSquaresTriad, main)
	}

	for _, want := range []string{
		"def r = resources(res)\n        chunked(us, rs, cs, t).collect { c, crs -> [m + r[c.name] + [chunks: cs.size()], k, fs, vs, us, crs, ids, ef, c.name, c, callStaged(m)] }",
		"[m + resources(res).join, k, fs, vs, us, joinRefs(",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitSplitNamesFilesByReference: a split and each chunk's main name
// a file their task staged by reference in the chunk defs and the chunk's
// outs, writing its records to -out-refs, which the driver resolves, as
// POSTED does, against the paths the task staged its items from: each
// chunk stages the files its def names, and the join those every chunk's
// def and outs name, but those in a dir the task stages whole.
func TestEmitSplitNamesFilesByReference(t *testing.T) {
	for _, want := range []string{
		"-args args -o chunks -out-refs chunk_refs\")}",
		"-o ${shq('chunkout/' + name)} -out-refs chunkout_refs\")}",
		"path('chunks/resources.tsv'), path('chunk_refs'), emit: split",
		"tuple val(meta), val(key), path('chunkout/*', type: 'dir'), eval('cat chunkout_refs'), emit: chunk",
		"return referred(cs, table.text, sources(us, rs, [], [])).collect { e -> [bundlesOf([e])[0], unstaged(rs + refsOf([e]), us)] }",
		"refsOf(referred([o], recs, sources(us, c[1], [], []) + [('chunk/' + o.name): c[0]]))",
		"return unstaged(rs + chunked.collectMany { e -> e[1] } + outs, us + chunked.collect { e -> e[0] } + os.collect { o -> o[0] })",
		"joinRefs(us, rs, chunked(us, rs, cs, t), os), ids, ef, cs, j, os.collect { o -> o[0] }, callStaged(m)]",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitReadsRecordsFromFiles (#549): a record of a line per fork or per
// chunk, PLAN's and a split's resources table and references, reaches the
// driver as a file it reads, not as an eval output, which Nextflow 26.04
// joins line by line with string concatenation (TaskEnvCollector.collect),
// quadratic in the lines; a task's reference records, which grow with a
// join's chunks or a fork root's forks, are an eval output of one line.
func TestEmitReadsRecordsFromFiles(t *testing.T) {
	main := readTree(t, emitFixture(t, "split_test"))["main.nf"]

	for _, rec := range []string{"nodes", "leaves", "files", "roots"} {
		if !strings.Contains(main, "path('"+rec+"'), emit: "+rec) || strings.Contains(main, "eval('cat "+rec+"')") {
			t.Errorf("PLAN does not output its %s record as a file:\n%s", rec, main)
		}
	}

	for _, rec := range []string{"chunks/resources.tsv", "chunk_refs"} {
		if !strings.Contains(stageModule, "path('"+rec+"')") || strings.Contains(stageModule, "eval('cat "+rec+"')") {
			t.Errorf("STAGE_SPLIT does not output its %s as a file", rec)
		}
	}

	if !strings.Contains(stageModule, "refs.tokenize(' ')") || strings.Contains(stageModule, "refs.tokenize('\\n')") {
		t.Error("the driver does not read a task's reference records, its eval outputs outs_refs, fork_refs and chunkout_refs, as one line")
	}
}

// TestEmitSizesNodesUnderOverrides: PLAN reads the run's mrp --overrides
// file, a launch param, and each task is scheduled with the cpus and
// memory PLAN resolved for its node, whose own overrides file its jobs
// resolve theirs under (split_test).
func TestEmitSizesNodesUnderOverrides(t *testing.T) {
	tree := readTree(t, emitFixture(t, "split_test"))

	for file, wants := range map[string][]string{
		"conf/base.config": {"params.mro2nf_overrides = null"},
		"main.nf": {
			"params.mro2nf_overrides ? file(params.mro2nf_overrides, checkIfExists: true) : []",
			"path overrides, stageAs: 'overrides/*', arity: '0..*'",
			"${overrides ? ' -overrides ' + shq(overrides[0]) : ''} -psid ${shq(meta.run.psid)}",
		},
		"stage.nf": {
			"${cmd} -overrides overrides.json",
			"[k[next], k, fs, es, planned(c, meta, r), r[1] == '1'",
			"cpus: d[3] as int, mem_gb: d[4] as double, overrides: d[5] == '-' ? '' : d[5], special: meta.special, digest: d[0]]",
		},
	} {
		for _, want := range wants {
			if !strings.Contains(tree[file], want) {
				t.Errorf("%s lacks %q:\n%s", file, want, tree[file])
			}
		}
	}
}

// TestEmitRetriesWhatMrpRetries: a task is retried, as many times as mrp
// retries a job, only on mre's transient exit or an executor kill or
// timeout (104, 130-145), never on a stage exception, an ASSERT or a failed
// mre step, which would fail again; and a retry requests the memory and cpus
// of the first attempt, as mrp retries a job at its request (#506).
func TestEmitRetriesWhatMrpRetries(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "split_test"))["conf/base.config"]

	for _, want := range []string{
		"errorStrategy = { task.exitStatus in ([75, 104] + (130..145)) ? 'retry' : 'terminate' }",
		"maxRetries = 2",
		"params.mro2nf_monitor = false\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("conf/base.config lacks %q:\n%s", want, cfg)
		}
	}

	if n := strings.Count(stageModule, `memory { "${meta.mem_gb} GB" }`); n != 4 || strings.Contains(stageModule, "attempt") {
		t.Errorf("stage.nf sizes %d of its 4 stage processes by their job's memory alone, or by the attempt", n)
	}
}

// TestEmitDeclaresTheEngineFloor: the config declares the oldest Nextflow
// the project runs on, with the `!` that fails an older launch rather than
// warning (#498), and so does the HealthOmics package's, from which
// HealthOmics chooses the engine. The nightly suite matrix runs this floor
// (TestNightlyNextflowFloor).
func TestEmitDeclaresTheEngineFloor(t *testing.T) {
	tree := readTree(t, emitFixture(t, "split_test"))

	const floor = "manifest.nextflowVersion = '!>=26.04.0'\n"

	for file, want := range map[string]string{
		"nextflow.config": "\n" + floor,
		"package.sh":      "cat > \"$pkg/nextflow.config\" <<EOF\n" + floor,
	} {
		if !strings.Contains(tree[file], want) {
			t.Errorf("%s lacks %q:\n%s", file, want, tree[file])
		}
	}
}

// TestEmitRefusesReservedParam: an entry input named like a param the
// project defines itself, or might, under the mro2nf_ prefix, is refused.
func TestEmitRefusesReservedParam(t *testing.T) {
	for _, name := range []string{"job_resources", "mro2nf_image"} {
		dir := t.TempDir()
		mro := filepath.Join(dir, "pipeline.mro")
		src := "stage S(\n    in  int " + name + ",\n    out int y,\n    src py \"s\",\n)\n\ncall S(\n    " + name + " = 1,\n)\n"

		if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}

		err := Emit(mro, Options{OutDir: t.TempDir(), MroPaths: []string{dir}})
		if !errors.Is(err, errReservedParam) {
			t.Errorf("Emit of input %s = %v, want errReservedParam", name, err)
		}
	}
}

// TestEmitTranspilesANullSourceLiteral (#621): a .mro whose own invocation
// has a null source, which makes a gate fix whether a map call's forks are
// null and the pipeline outs read it, transpiles: the project's topology
// holds the reads of the witness with that input null, and the map call's
// root has no fork whatever the gate holds.
func TestEmitTranspilesANullSourceLiteral(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "rt_gate_split", "pipeline.mro"))
	if err != nil {
		t.Fatal(err)
	}

	decls, _, ok := strings.Cut(string(raw), "call RTG(")
	if !ok {
		t.Fatal("rt_gate_split has no call of RTG")
	}

	src := decls + "call RTG(\n    some = null,\n    skip = false,\n)\n"

	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")

	if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(mro, Options{OutDir: filepath.Join(t.TempDir(), "proj"), MroPaths: []string{dir}}); err != nil {
		t.Errorf("Emit = %v", err)
	}
}

// TestEmitRefusesKeywordInput (#585): an entry input named with a word
// Nextflow's params block refuses, a keyword of its lexer (def) or a Groovy
// keyword its parser refuses (class, volatile, which Martian lexes as its
// own keyword), fails the transpile naming the input and the word, and
// emits nothing.
func TestEmitRefusesKeywordInput(t *testing.T) {
	for _, name := range []string{"def", "class", "volatile"} {
		dir := t.TempDir()
		mro := filepath.Join(dir, "pipeline.mro")
		src := "stage S(\n    in  int " + name + ",\n    out int y,\n    src py \"s\",\n)\n\ncall S(\n    " + name + " = 1,\n)\n"

		if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}

		out := filepath.Join(t.TempDir(), "proj")
		want := fmt.Sprintf("entry input %s: %q is a Groovy or Nextflow keyword", name, name)

		if err := Emit(mro, Options{OutDir: out, MroPaths: []string{dir}}); !errors.Is(err, errKeywordParam) || !strings.Contains(err.Error(), want) {
			t.Errorf("Emit of input %s = %v, want an error naming %q", name, err, want)
		}

		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused emit of input %s left %s (stat: %v)", name, out, err)
		}
	}
}

// TestEmitStagesEntryFilesIntoTheirReaders: a node whose bindings read an
// entry file stages, of PLAN's entry files, those listed for its fork of its
// entry-fed roots; a node that reads none stages none and joins no entry
// record (entry_filearr: COUNTARR reads reads; diamond_min has no file).
func TestEmitStagesEntryFilesIntoTheirReaders(t *testing.T) {
	arr := byNode(t, "entry_filearr")

	for _, want := range []string{
		"entry = ENTRY(PLAN.out.files, PLAN.out.leaves)",
		"n<COUNTARR> = INPUT_ENTRY(b<COUNTARR>.run, entry, [], [])",
	} {
		if !strings.Contains(arr, want) {
			t.Errorf("entry_filearr's main.nf lacks %q:\n%s", want, arr)
		}
	}

	plain := byNode(t, "diamond_min")
	if strings.Contains(plain, "entry =") || !strings.Contains(plain, "n<ADD> = INPUT(u<ADD>, [['y']])") {
		t.Errorf("diamond_min reads no entry file, yet stages some:\n%s", plain)
	}

	for _, want := range []string{
		"val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')",
		"-leaves ${shq(leaves.join(','))}",
		"file(new String(f[1].decodeBase64(), 'UTF-8'), glob: false)",
		"def ids = ef.files[v[4].node + ' ' + (v[1][at].join('.') ?: '-')] ?: []",
		"inputOf(v, fields, ids, ids.collect { x -> ef.leaves[x] })",
		"out = inst.map { v -> inputOf(v, fields, [], []) }",
	} {
		if strings.Count(stageModule, want) < 1 {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitStagesEntryFilesIntoOuts: pipeline outs that return an entry
// file value stage it into OUTS, as PLAN lists it by the pipeline's fqid,
// for post-processing to publish it from inside a container; outs that
// read none stage none (diamond_min).
func TestEmitStagesEntryFilesIntoOuts(t *testing.T) {
	dir := t.TempDir()
	mro := filepath.Join(dir, "pipeline.mro")
	src := "filetype txt;\n\npipeline ECHO(\n    in  txt ref,\n    out txt echoed,\n)\n{\n    return (\n        echoed = self.ref,\n    )\n}\n\ncall ECHO(\n    ref = \"/data/ref.txt\",\n)\n"

	if err := os.WriteFile(mro, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := Emit(mro, Options{OutDir: out, MroPaths: []string{dir}, Version: "test"}); err != nil {
		t.Fatal(err)
	}

	main := readTree(t, out)["main.nf"]
	for _, want := range []string{
		"entry = ENTRY(PLAN.out.files, PLAN.out.leaves)",
		"outs_in.map { xs -> recordsOf(xs) }, entry.map { ef -> def ids = ef.files['ECHO -'] ?: []; [ids, ids.collect { x -> ef.leaves[x] }] }, file(",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("ECHO's main.nf lacks %q:\n%s", want, main)
		}
	}

	if plain := readTree(t, emitFixture(t, "diamond_min"))["main.nf"]; !strings.Contains(plain, "outs_in.map { xs -> recordsOf(xs) }, channel.value([[], []]), file(") {
		t.Errorf("diamond_min's outs read no entry file, yet OUTS stages some:\n%s", plain)
	}

	for _, want := range []string{"    val values\n    tuple val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')\n", "-locations locations${leaves ? ' -leaves ' + shq(leaves.join(',')) : ''} -manifest publish/publish.json"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("OUTS lacks %q", want)
		}
	}
}

// TestEmitZipsByToken: an instance zipping another stage's output gets
// that stage's partner element at its own fork's index token, or the
// absent record, never the whole output (zip_runtime: LONG zips THREE.xs;
// zip_runtime_map: BY_AX zips ABC.m by key token).
func TestEmitZipsByToken(t *testing.T) {
	for _, c := range []struct{ fixture, fork, zip string }{
		{"zip_runtime", "i<LONG> = FORK(channel.of([[], [], [], []]), ELEMENTS(o<TWO>_forks, 'ZIP_RUNTIME.LONG', [], false), [], true)", "i<LONG> = ZIP(i<LONG>, o<THREE>_forks, [], 'ZIP_RUNTIME.LONG@ZIP_RUNTIME.THREE.xs', 0, 'none')"},
		{"zip_runtime_map", "i<BY_AX> = FORK(channel.of([[], [], [], []]), ELEMENTS(o<AX>_forks, 'ZIP_RUNTIME_MAP.BY_AX', [], true), [], true)", "i<BY_AX> = ZIP(i<BY_AX>, o<ABC>_forks, [], 'ZIP_RUNTIME_MAP.BY_AX@ZIP_RUNTIME_MAP.ABC.m', 0, 'none')"},
	} {
		main := byNode(t, c.fixture)

		for _, want := range []string{c.fork, c.zip} {
			if !strings.Contains(main, want) {
				t.Errorf("%s's main.nf lacks %q:\n%s", c.fixture, want, main)
			}
		}

		if strings.Contains(main, "READ(") {
			t.Errorf("%s: an instance stages a whole output:\n%s", c.fixture, main)
		}
	}

	for _, want := range []string{
		"keyed ? name.tokenize('@')[1] : t[-1].toString()",
		"[k, fs, es + ([m[tk[token]] ?: m[absent]] - [null]), tk]",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitGatesBySet: PLAN's statically disabled fork keys of a node and a
// gating instance's posted gates are each parsed once, into a set, so the
// gate of each of a node's N instances is a lookup, not a parse or scan of
// N keys (split_gate_entry: SRC gates INNER.WORK and KEYED.WORK, its gates
// parsed once for both). TestBench's forkdisabled lanes run both paths.
func TestEmitGatesBySet(t *testing.T) {
	main := byNode(t, "split_gate_entry")

	for _, want := range []string{
		"o<SRC>_gates = GATES(N<SRC>.out.posted, b<SRC>.skip)",
		"b<INNER.WORK> = ENABLED(GATED(g<INNER.WORK>, o<SRC>_gates), [[[0], 'SPLIT_GATE_ENTRY.INNER.WORK@SPLIT_GATE_ENTRY.INNER@']], [])",
		"b<KEYED.WORK> = ENABLED(GATED(g<KEYED.WORK>, o<SRC>_gates), [[[0], 'SPLIT_GATE_ENTRY.KEYED.WORK@SPLIT_GATE_ENTRY.KEYED@']], [])",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("split_gate_entry's wiring lacks %q:\n%s", want, main)
		}
	}

	mains := wiringMains()

	for name, want := range map[string]string{
		"NODES":   "[(f[0]): [f[1], f[2], f[3].tokenize(',').toSet()] + f.drop(4)]",
		"GATE":    "r[1] == '1' || r[2].contains(k.join('.'))",
		"GATES":   "posted.map { v -> [v[0], v[7].tokenize(' ').toSet()] }.mix(skip.map { v -> [v[1], [].toSet()] })",
		"ENABLED": "skip: disabled(v, gates)",
		"REGATE":  "[disabled(v, gates)]",
	} {
		if !strings.Contains(mains[name], want) {
			t.Errorf("stage.nf's %s lacks %q:\n%s", name, want, mains[name])
		}
	}

	if want := "return v[5] || [gss, gates].transpose().any { gs, g -> gs.contains(g[1] + v[1][g[0]].join('.')) }"; !strings.Contains(stageModule, want) {
		t.Errorf("stage.nf lacks %q", want)
	}

	for _, name := range []string{"GATE", "GATED", "REGATE", "ENABLED"} {
		if mains[name] == "" || strings.Contains(mains[name], "tokenize") {
			t.Errorf("stage.nf's %s parses a record per instance, or is missing:\n%s", name, mains[name])
		}
	}
}

// TestEmitForkedGatherWaitsForItsUpstream: a forked call's gather of its
// share of an inner map call, whose root the entry (BY_ENTRY) or a stage
// (BY_STAGE, by MKLIST) feeds, is the plain gather over all of its
// upstream, as mrp waits for every fork of a node a node reads
// (martian/core/node.go getState), so PLAN writes no fork counts.
// TestMrpDiff/map_nested_gather runs the fixture against mrp.
func TestEmitForkedGatherWaitsForItsUpstream(t *testing.T) {
	main := byNode(t, "map_nested_gather")

	for _, want := range []string{
		"u<BY_ENTRY.TOTAL> = READ(b<BY_ENTRY.TOTAL>.run, GATHER(b<BY_ENTRY.TOTAL>.run, o<BY_ENTRY.SQ>_outs, [0], [0], ['y']))",
		"u<BY_STAGE.TOTAL> = READ(u<BY_STAGE.TOTAL>, GATHER(b<BY_STAGE.TOTAL>.run, o<BY_STAGE.SQ>_outs, [0], [0], ['y']))",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("map_nested_gather's main.nf lacks %q:\n%s", want, main)
		}
	}

	for _, unwanted := range []string{"counts", "COUNTS", "groupKey"} {
		if strings.Contains(main, unwanted) {
			t.Errorf("map_nested_gather's main.nf sizes a gather (%q):\n%s", unwanted, main)
		}
	}
}

// TestEmitReadsWhatEachInstanceReads (#601): a read some of whose
// instances read only some upstream instances, an element of a literal
// (map_chain_mixed) or two roots each aligned with the upstream's
// (map_chain_cross), joins each instance to those through NEEDS, never a
// GATHER of every upstream instance. TestStandInStagesOnlyWhatItReads
// holds what each is given to what it reads.
func TestEmitReadsWhatEachInstanceReads(t *testing.T) {
	for _, fixture := range []string{"map_chain_mixed", "map_chain_cross"} {
		main := wiringNF(readTree(t, emitFixture(t, fixture)))

		if !strings.Contains(main, "NEEDS(") || strings.Contains(main, "GATHER(") {
			t.Errorf("%s's main.nf reads its upstream other than through NEEDS:\n%s", fixture, main)
		}
	}
}

// TestEmitGathersAReadAlsoAligned (#622): a read of an upstream both at the
// reader's own fork and whole (bench/mapgather: B_LIT and B_RUN read A_LIT
// and A_RUN as f = split A.f and all = A.f) is the one GATHER of every
// upstream instance, whose whole read covers the aligned one, never NEEDS,
// whose group per reader instance would hold each upstream instance once
// per pattern.
func TestEmitGathersAReadAlsoAligned(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "bench", "mapgather"))
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := Emit(filepath.Join(dir, "pipeline_w4.mro"), Options{OutDir: out, MroPaths: []string{dir}, Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}

	main := wiringNF(readTree(t, out))

	if strings.Contains(main, "NEEDS(") || strings.Count(main, "GATHER(") != 2 {
		t.Errorf("mapgather's main.nf reads its upstreams other than through one GATHER each:\n%s", main)
	}
}

// TestEmitReadsOutsAsTheyCome (#559): a node's outs are keyed by its whole
// key, which a reader sharing every root of it has at its own positions,
// so the reader combines the node's outs channel as it comes, and its
// process's INPUT picks the outputs each read reads; keying or selecting
// them per read cost CellRanger 1,263 edges of Nextflow's DAG
// (map_nested_gather: BY_STAGE.TOTAL reads MKLIST's xs at its fork of their
// map root, and gathers SQ's y).
func TestEmitReadsOutsAsTheyCome(t *testing.T) {
	main := byNode(t, "map_nested_gather")

	want := "u<BY_STAGE.TOTAL> = READ(b<BY_STAGE.TOTAL>.run, o<BY_STAGE.MKLIST>_outs)\n" +
		"    u<BY_STAGE.TOTAL> = READ(u<BY_STAGE.TOTAL>, GATHER(b<BY_STAGE.TOTAL>.run, o<BY_STAGE.SQ>_outs, [0], [0], ['y']))\n" +
		"    n<BY_STAGE.TOTAL> = INPUT(u<BY_STAGE.TOTAL>, [['xs'], null])"
	if !strings.Contains(main, want) {
		t.Errorf("map_nested_gather's main.nf lacks %q:\n%s", want, main)
	}

	mains := wiringMains()

	if want = "out = inst.combine(ups, by: 0)\nout"; mains["READ"] != want {
		t.Errorf("stage.nf's READ's statements are %q, want %q", mains["READ"], want)
	}

	if _, ok := mains["SELECT"]; ok {
		t.Errorf("stage.nf keeps SELECT, a step per read:\n%s", mains["SELECT"])
	}
}

// TestEmitKeysReadsWhereTheirRootsChange (#559): an enabled instance leads
// with its ordinals of the roots its node's first read joins on, a read is
// one combine, which adds the node's outs after what the instance read
// before, and REKEY keys the instance again, with the outputs each read
// since reads, only before a read joining on other roots; a map per read
// to key the instance cost CellRanger 1,135 edges of Nextflow's DAG
// beyond those (gate_chain: WORK reads at no fork, twice at its own fork
// of their map root, position 0, and at none). TestMrpDiff/gate_chain runs
// it against mrp.
func TestEmitKeysReadsWhereTheirRootsChange(t *testing.T) {
	got := assignments(byNode(t, "gate_chain"), "u<INNER.WORK>", "n<INNER.WORK>")

	want := []string{
		"u<INNER.WORK> = READ(b<INNER.WORK>.run, o<BASE>_outs)",
		"u<INNER.WORK> = READ(REKEY(u<INNER.WORK>, [['y']], [0]), o<INNER.ADD>_outs)",
		"u<INNER.WORK> = READ(u<INNER.WORK>, o<INNER.FLAG>_outs)",
		"u<INNER.WORK> = READ(REKEY(u<INNER.WORK>, [['y'], ['off']], []), o<OUTER>_outs)",
		"n<INNER.WORK> = INPUT(u<INNER.WORK>, [['off']])",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gate_chain's WORK (-want +got):\n%s", diff)
	}

	mains := wiringMains()

	for name, want := range map[string]string{
		"ENABLED": "run = b.run.map { v -> [v[1][at]] + v.subList(1, 5) + [[]] }",
		"REKEY":   "out = inst.map { v -> [v[1][next]] + v.subList(1, 5) + [readItems(v, fields)] }",
		"GATHER":  "seeds = run.map { v -> [v[1][mine], null] }.unique()",
		"INPUT":   "out = inst.map { v -> inputOf(v, fields, [], []) }",
	} {
		if !strings.Contains(mains[name], want) {
			t.Errorf("stage.nf's %s lacks %q:\n%s", name, want, mains[name])
		}
	}

	if want := "return v[5] + [dss, fields].transpose().collectMany { ds, f -> selected(ds, f) }"; !strings.Contains(stageModule, want) {
		t.Errorf("stage.nf lacks %q", want)
	}
}

// TestEmitKeysGatesWhereTheirRootsChange (#559): GATE keys an instance by
// its ordinals of the roots of the stage its node's first gate joins on,
// a gate is one combine, which adds the gates that stage posted there, and
// REGATE keys the instance again, deciding it from the gates since, only
// before a gate joining on other roots, as ENABLED decides it from the
// last; a map per gate to key the instance cost CellRanger 831 edges of
// Nextflow's DAG (gate_chain: WORK is gated per fork of their map root,
// position 0, by INNER's FLAG, then at no fork by OUTER).
// TestMrpDiff/gate_chain runs it against mrp.
func TestEmitKeysGatesWhereTheirRootsChange(t *testing.T) {
	got := assignments(byNode(t, "gate_chain"), "g<INNER.WORK>", "b<INNER.WORK>")

	want := []string{
		"g<INNER.WORK> = GATE(i<INNER.WORK>, nodes, [node: 'GATE_CHAIN.INNER.WORK', special: ''], [0])",
		"g<INNER.WORK> = GATED(g<INNER.WORK>, o<INNER.FLAG>_gates)",
		"b<INNER.WORK> = ENABLED(GATED(REGATE(g<INNER.WORK>, [[[], 'GATE_CHAIN.INNER.WORK@@']], []), o<OUTER>_gates), [[[], 'GATE_CHAIN.INNER.WORK@@']], [])",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gate_chain's WORK (-want +got):\n%s", diff)
	}

	mains := wiringMains()

	for name, want := range map[string]string{
		"GATE":    "[k[next], k, fs, es, planned(c, meta, r), ",
		"GATED":   "out = gated.combine(gates, by: 0)\nout",
		"REGATE":  "out = gated.map { v -> [v[1][next]] + v.subList(1, 5) + [disabled(v, gates)] }",
		"ENABLED": "skip: disabled(v, gates)",
		"GATES":   "skip.map { v -> [v[1], [].toSet()] }",
		"POSTED":  "skip.map { v -> [v[1], []] }",
	} {
		if !strings.Contains(mains[name], want) {
			t.Errorf("stage.nf's %s lacks %q:\n%s", name, want, mains[name])
		}
	}
}

// assignments are the statements of main, trimmed, that assign one of
// names, in order.
func assignments(main string, names ...string) []string {
	var out []string

	for l := range strings.SplitSeq(main, "\n") {
		l = strings.TrimSpace(l)
		if slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(l, n+" = ") }) {
			out = append(out, l)
		}
	}

	return out
}

// wiringMains is the statements of each workflow of the stage module, its
// main and its emits, by name.
func wiringMains() map[string]string {
	mains := map[string]string{}
	for _, w := range parseWorkflows("stage.nf", stageModule) {
		mains[w.name] = strings.Join(slices.Concat(w.main, w.emits), "\n")
	}

	return mains
}

// wiringNF is a project's main.nf and the modules of its sub-workflows.
func wiringNF(tree map[string]string) string {
	var b strings.Builder

	for _, name := range slices.Sorted(maps.Keys(tree)) {
		if name == "main.nf" || strings.HasPrefix(name, subworkflowDir+"/") {
			b.WriteString(tree[name])
		}
	}

	return b.String()
}

var (
	numberedAlias   = regexp.MustCompile(`\bN(\d+)_\w+`)
	numberedHold    = regexp.MustCompile(`\bnodes_after((?:_\d+)+)\b`)
	numberedChannel = regexp.MustCompile(`(^|[\s(\[,])([a-z])(\d+)(_outs|_forks|_gates)?\b`)
)

// byNode is fixture's project's wiring (wiringNF) with each name the emitter
// numbers by its node's place in the dependency order, a process alias
// (N0_GEN), a node's channels (b0, o0_outs) and a hold of its preflights
// (nodes_after_0_2), naming the node by its fqid less the pipeline's own id
// (N<GEN>, b<GEN>, o<GEN>_outs, nodes_after<TOP_CHECK><SUB.CHECK>): a pin
// names the nodes it wires, whatever their order.
func byNode(t *testing.T, fixture string) string {
	t.Helper()

	dir := fixturePath(t, fixture)

	src, err := cg.ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	p, err := newProject(src, Options{MroPaths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}

	node := func(digits string) (int, string) {
		i, err := strconv.Atoi(digits)
		if err != nil || i >= len(p.topo.Nodes) {
			t.Fatalf("%s: no node %s", fixture, digits)
		}

		fqid := p.topo.Nodes[i].Fqid
		if _, rest, ok := strings.Cut(fqid, "."); ok {
			return i, "<" + rest + ">"
		}

		return i, "<" + fqid + ">"
	}

	nf := numberedAlias.ReplaceAllStringFunc(wiringNF(readTree(t, emitFixture(t, fixture))), func(m string) string {
		i, name := node(numberedAlias.FindStringSubmatch(m)[1])

		return "N" + name + strings.TrimPrefix(m, alias(i, p.topo.Nodes[i].Fqid))
	})
	nf = numberedHold.ReplaceAllStringFunc(nf, func(m string) string {
		out := "nodes_after"
		var outSb1189 strings.Builder
		for d := range strings.SplitSeq(strings.TrimPrefix(numberedHold.FindStringSubmatch(m)[1], "_"), "_") {
			_, name := node(d)
			outSb1189.WriteString(name)
		}
		out += outSb1189.String()

		return out
	})

	return numberedChannel.ReplaceAllStringFunc(nf, func(m string) string {
		s := numberedChannel.FindStringSubmatch(m)
		_, name := node(s[3])

		return s[1] + s[2] + name + s[4]
	})
}

// TestEmitWaitsForPreflights: a preflight signals once every instance of it
// is done; PLAN's node records are held until it is, after the preflights
// before it some node also waits for; and each node gates its instances on
// the records held for its own preflights (preflight_order: TOP_CHECK gates
// every other call, SUB's CHECK gates WORK).
func TestEmitWaitsForPreflights(t *testing.T) {
	main := byNode(t, "preflight_order")

	var got []string

	for l := range strings.SplitSeq(main, "\n") {
		if strings.Contains(l, "GATE(") || strings.Contains(l, "DONE(") || strings.Contains(l, "AFTER(") {
			got = append(got, strings.TrimSpace(l))
		}
	}

	want := []string{
		"b<TOP_CHECK> = ENABLED(GATE(channel.of([[], [], [], []]), nodes, [node: 'TOP.TOP_CHECK', special: ''], []), [], [])",
		"d<TOP_CHECK> = DONE(N<TOP_CHECK>.out.posted)",
		"nodes_after<TOP_CHECK> = AFTER(nodes, d<TOP_CHECK>)",
		"b<EMIT> = ENABLED(GATE(channel.of([[], [], [], []]), nodes_after<TOP_CHECK>, [node: 'TOP.EMIT', special: ''], []), [], [])",
		"b<SUB.CHECK> = ENABLED(GATE(channel.of([[], [], [], []]), nodes_after<TOP_CHECK>, [node: 'TOP.SUB.CHECK', special: ''], []), [], [])",
		"d<SUB.CHECK> = DONE(N<SUB.CHECK>.out.posted)",
		"nodes_after<TOP_CHECK><SUB.CHECK> = AFTER(nodes_after<TOP_CHECK>, d<SUB.CHECK>)",
		"b<SUB.WORK> = ENABLED(GATE(channel.of([[], [], [], []]), nodes_after<TOP_CHECK><SUB.CHECK>, [node: 'TOP.SUB.WORK', special: ''], []), [], [])",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("preflight_order's gates and holds (-want +got):\n%s", diff)
	}
}
