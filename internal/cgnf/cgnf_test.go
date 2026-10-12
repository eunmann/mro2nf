package cgnf

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
// config and its container profiles' configs, the type manifest, a copy of
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
		filepath.Join("_assets/mro", entry), "_assets/types.json", "conf/container.config", "conf/healthomics.config",
		"main.nf", "nextflow.config", "package.sh", "parameter-template.json", "stage.nf",
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
	cfg := readTree(t, emitFixture(t, "split_test"))["nextflow.config"]

	for _, want := range []string{"params.mro2nf_mre = 'mre'\n", "params.mro2nf_mrjob = null\n", "params.mro2nf_adapters = null\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	for _, want := range []string{
		"if (params.mro2nf_adapters) {\n        cmd += \" -adapters ${shq(params.mro2nf_adapters)}\"",
		"if (params.mro2nf_mrjob) {\n        cmd += \" -mrjob ${shq(params.mro2nf_mrjob)}\"",
	} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitConfinesAtLaunch: a stage task's outputs are confined to its
// work dir (mre run -confine) only when the run sets params.mro2nf_confine.
func TestEmitConfinesAtLaunch(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "file_min"))["nextflow.config"]

	if !strings.Contains(cfg, "params.mro2nf_confine = false\n") {
		t.Errorf("nextflow.config does not leave confinement off:\n%s", cfg)
	}

	if !strings.Contains(stageModule, "if (on(params.mro2nf_confine)) {\n        cmd += ' -confine'") {
		t.Error("stage.nf does not confine on params.mro2nf_confine")
	}
}

// TestEmitAliases: every stage node of the witness's call graph is one
// alias of the stage module, a split stage the split, main and join
// processes, and every entry-fed fork root is one PLAN output
// (map_fork_upstream: GEN, then DBL forked on GEN's map).
func TestEmitAliases(t *testing.T) {
	main := readTree(t, emitFixture(t, "split_test"))["main.nf"]

	if !strings.Contains(main, "include { STAGE_SPLIT as N0_SUM_SQUARES_SP ; STAGE_MAIN as N0_SUM_SQUARES_MN ; STAGE_JOIN as N0_SUM_SQUARES_JN } from './stage.nf'") {
		t.Errorf("split_test's split stage is not the split triad:\n%s", main)
	}

	up := readTree(t, emitFixture(t, "map_fork_upstream"))["main.nf"]

	for _, want := range []string{
		"include { STAGE as N0_GEN } from './stage.nf'",
		"include { STAGE as N1_DBL } from './stage.nf'",
		"o0_forks.flatMap { _kp, ds -> ds.findAll { e -> e.parent.name == 'MFU.DBL' }",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("map_fork_upstream's main.nf lacks %q:\n%s", want, up)
		}
	}

	if strings.Contains(up, "emit: r0") {
		t.Errorf("map_fork_upstream has no entry-fed root, yet PLAN emits one:\n%s", up)
	}

	nested := readTree(t, emitFixture(t, "map_pipe_nested"))["main.nf"]
	if !strings.Contains(nested, "emit: r0") || !strings.Contains(nested, "emit: r1") {
		t.Errorf("map_pipe_nested's two entry-fed roots are not two PLAN outputs:\n%s", nested)
	}
}

// TestEmitGPURequest: every task's GPU request follows the special it is
// scheduled with, as its clusterOptions do, in one config rule and no
// per-alias one (TestGPURequestFollowsSpecial runs it).
func TestEmitGPURequest(t *testing.T) {
	tree := readTree(t, emitFixture(t, "gpu_stage"))

	if !strings.Contains(tree["nextflow.config"], "    accelerator = { def s = meta.special ?: ''; ") {
		t.Errorf("nextflow.config lacks the accelerator rule:\n%s", tree["nextflow.config"])
	}

	if strings.Contains(tree["nextflow.config"], "withName:") {
		t.Errorf("nextflow.config names a process:\n%s", tree["nextflow.config"])
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
	split := readTree(t, emitFixture(t, "split_test"))["main.nf"]
	gather := readTree(t, emitFixture(t, "fork_mid"))["main.nf"]

	for _, c := range []struct{ fixture, main, want string }{
		{"split_test", split, "groupTuple().map { gk, os -> [gk.getGroupTarget(), os.sort(false) { a, b -> a.name <=> b.name }] }"},
		{"fork_mid", gather, ".collect { e -> [ku, e] }] }).groupTuple().map { c, dss -> [c, ordered(dss.findAll { x -> x != null }.collectMany { x -> x })] }"},
		{"split_test", split, "o0_outs.flatMap { k, ds -> ds.findAll { e -> e.name in ['sum'] }.collect { e -> [[0] + k, e] } }).toList().map { xs -> ordered(xs) }"},
	} {
		if !strings.Contains(c.main, c.want) {
			t.Errorf("%s's main.nf lacks %q:\n%s", c.fixture, c.want, c.main)
		}
	}
}

// TestEmitLocatesOutsInputs: OUTS gets, beside each outs bundle it stages,
// where its producing task's work dir keeps it, which names a file the
// pipeline outs leave unpublished (#457), and passes both to mre outs.
func TestEmitLocatesOutsInputs(t *testing.T) {
	main := readTree(t, emitFixture(t, "nested_path_unpublished"))["main.nf"]

	if !strings.Contains(main, "OUTS(call.map { c -> [entry: ") || !strings.Contains(main, "outs_in, outs_in.map { ds -> ds.collect { d -> d.toUriString() } }, channel.value([[], []]), file(") {
		t.Errorf("OUTS is not given its inputs' locations:\n%s", main)
	}

	for _, want := range []string{"path ups, stageAs: 'in/u*', arity: '0..*'\n    val locs", "${locations(ups, locs)}", "-in in -locations locations"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("OUTS lacks %q", want)
		}
	}
}

// TestEmitWritesTheCallToAFile: every process that runs mre writes the
// run's call, its value, to the file mre reads it from, before mre runs, as
// a wide run's call would pass the kernel's 128 KiB limit on one argument.
func TestEmitWritesTheCallToAFile(t *testing.T) {
	if !strings.Contains(stageModule, "\"${code(meta, root)} -call call.b64\"") || !strings.Contains(stageModule, "\"cat > call.b64 <<'MRO2NF_CALL'\\n${meta.call}\\nMRO2NF_CALL\"") {
		t.Error("stage.nf does not hand mre the call in a file")
	}

	for _, proc := range []string{"STAGE", "STAGE_SPLIT", "STAGE_MAIN", "STAGE_JOIN", "OUTS"} {
		_, body, _ := strings.Cut(stageModule, "\nprocess "+proc+" {")
		_, script, _ := strings.Cut(body, "script:\n")
		script, _, _ = strings.Cut(script, "\n}\n")

		if at := strings.Index(script, "${callFile(meta)}"); at < 0 || at != strings.Index(script, "${") {
			t.Errorf("%s does not write the call before it runs mre:\n%s", proc, script)
		}
	}
}

// TestEmitStagesChunkApart: a chunk's main stages the split's chunk dir
// below chunk/, apart from the job's metadata dir of the chunk's name, so
// the job writes nothing into the split's outputs, which would change their
// -resume key (split_test).
func TestEmitStagesChunkApart(t *testing.T) {
	if !strings.Contains(stageModule, "tuple val(meta), val(key), path('args'), val(name), path(chunk, stageAs: 'chunk/*')") {
		t.Error("STAGE_MAIN does not stage its chunk below chunk/")
	}
}

// TestEmitSizesChunksAndJoin: each chunk's main and the join are scheduled
// with the resources their jobs get, the split's resources table, a value
// the driver parses no record for (join_resources: the join requests 2
// threads and 3 GB; TestWriteResources writes the table).
func TestEmitSizesChunksAndJoin(t *testing.T) {
	main := readTree(t, emitFixture(t, "join_resources"))["main.nf"]

	for _, want := range []string{
		"def r = resources(res); cs.collect { c -> [m + r[c.name] + [chunks: cs.size()], k, a, c.name, c] }",
		"s0.map { m, k, a, cs, j, res -> [k, m + resources(res).join, a, cs, j] }",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("join_resources's main.nf lacks %q:\n%s", want, main)
		}
	}
}

// TestEmitSizesNodesUnderOverrides: PLAN reads the run's mrp --overrides
// file, a launch param, and each task is scheduled with the cpus and
// memory PLAN resolved for its node, whose own overrides file its jobs
// resolve theirs under (split_test).
func TestEmitSizesNodesUnderOverrides(t *testing.T) {
	tree := readTree(t, emitFixture(t, "split_test"))

	for file, wants := range map[string][]string{
		"nextflow.config": {"params.mro2nf_overrides = null"},
		"main.nf": {
			"params.mro2nf_overrides ? file(params.mro2nf_overrides, checkIfExists: true) : []",
			"path overrides, stageAs: 'overrides/*', arity: '0..*'",
			"${overrides ? ' -overrides ' + shq(overrides[0]) : ''} -psid ${shq(params.mro2nf_psid)}",
			"cpus: d[3] as int, mem_gb: d[4] as double, overrides: (d[5] == '-' ? '' : d[5])",
		},
		"stage.nf": {"${cmd} -overrides overrides.json"},
	} {
		for _, want := range wants {
			if !strings.Contains(tree[file], want) {
				t.Errorf("%s lacks %q:\n%s", file, want, tree[file])
			}
		}
	}
}

// TestEmitRetriesWhatMrpRetries: a task is retried, as many times as mrp
// retries a job, only on mre's transient exit or a signal's, never on a
// stage exception, an ASSERT or a failed mre step; and under monitor, a
// launch param off by default, a retry keeps the stage's own memory, at
// which mrjob kills it as mrp does.
func TestEmitRetriesWhatMrpRetries(t *testing.T) {
	cfg := readTree(t, emitFixture(t, "split_test"))["nextflow.config"]

	for _, want := range []string{
		"errorStrategy = { task.exitStatus in [75, 137, 143] ? 'retry' : 'terminate' }",
		"maxRetries = 2",
		"params.mro2nf_monitor = false\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	if !strings.Contains(stageModule, `"${on(params.mro2nf_monitor) ? meta.mem_gb : meta.mem_gb * attempt} GB"`) {
		t.Error("stage.nf escalates a retry's memory under monitor")
	}
}

// TestEmitRefusesReservedParam: an entry input named like a param the
// project defines itself, or might, under the mro2nf_ prefix, is refused.
func TestEmitRefusesReservedParam(t *testing.T) {
	for _, name := range []string{"outdir", "mro2nf_image"} {
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

// TestEmitStagesEntryFilesIntoTheirReaders: a node whose bindings read an
// entry file stages, of PLAN's entry files, those listed for its fork of its
// entry-fed roots; a node that reads none stages none and joins no entry
// record (entry_filearr: COUNTARR reads reads; diamond_min has no file).
func TestEmitStagesEntryFilesIntoTheirReaders(t *testing.T) {
	arr := readTree(t, emitFixture(t, "entry_filearr"))["main.nf"]

	for _, want := range []string{
		"entry = PLAN.out.files.combine(PLAN.out.leaves)",
		"file(new String(f[1].decodeBase64(), 'UTF-8'), glob: false)",
		"N0_COUNTARR(u0.combine(call).combine(entry).map { k, fs, es, d, us, c, ef -> def ids = ef.files['EPA.COUNTARR' + ' ' + ([].join('.') ?: '-')] ?: [];",
	} {
		if !strings.Contains(arr, want) {
			t.Errorf("entry_filearr's main.nf lacks %q:\n%s", want, arr)
		}
	}

	plain := readTree(t, emitFixture(t, "diamond_min"))["main.nf"]
	if strings.Contains(plain, "entry =") || !strings.Contains(plain, "k, fs, es + us, [], []]") {
		t.Errorf("diamond_min reads no entry file, yet stages some:\n%s", plain)
	}

	for _, want := range []string{"val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')", "-leaves ${shq(leaves.join(','))}"} {
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
		"entry = PLAN.out.files.combine(PLAN.out.leaves)",
		"outs_in.map { ds -> ds.collect { d -> d.toUriString() } }, entry.map { ef -> def ids = ef.files['ECHO -'] ?: []; [ids, ids.collect { x -> ef.leaves[x] }] }, file(",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("ECHO's main.nf lacks %q:\n%s", want, main)
		}
	}

	if plain := readTree(t, emitFixture(t, "diamond_min"))["main.nf"]; !strings.Contains(plain, "outs_in.map { ds -> ds.collect { d -> d.toUriString() } }, channel.value([[], []]), file(") {
		t.Errorf("diamond_min's outs read no entry file, yet OUTS stages some:\n%s", plain)
	}

	for _, want := range []string{"    val locs\n    tuple val(leaves), path(entry, stageAs: 'entry/?/*', arity: '0..*')\n", "-locations locations${leaves ? ' -leaves ' + shq(leaves.join(',')) : ''} -o results"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("OUTS lacks %q", want)
		}
	}
}

// TestEmitZipsByToken: an instance zipping another stage's output stages
// that stage's partner element at its own fork's index token, or the
// absent record, never the whole output (zip_runtime: LONG zips THREE.xs;
// zip_runtime_map: BY_AX zips ABC.m by key token).
func TestEmitZipsByToken(t *testing.T) {
	for _, c := range []struct{ fixture, root, partner, token string }{
		{"zip_runtime", "ZIP_RUNTIME.LONG", "ZIP_RUNTIME.LONG@ZIP_RUNTIME.THREE.xs", "t[-1].toString()"},
		{"zip_runtime_map", "ZIP_RUNTIME_MAP.BY_AX", "ZIP_RUNTIME_MAP.BY_AX@ZIP_RUNTIME_MAP.ABC.m", "e.name.tokenize('@')[1]"},
	} {
		main := readTree(t, emitFixture(t, c.fixture))["main.nf"]

		for _, want := range []string{
			"e.parent.name == '" + c.root + "' }.collect { e -> def t = e.name.tokenize('@')[0].tokenize('.').collect { x -> x as int }; [[], t[-1], e, " + c.token + "] }",
			"ds.findAll { e -> e.parent.name == '" + c.partner + "' }.collectEntries { e -> [(e.name): e] }] }, by: 0).map { _q, k, fs, es, tk, m -> [k, fs, es + ([m[tk[0]] ?: m['none']] - [null]), tk] }",
		} {
			if !strings.Contains(main, want) {
				t.Errorf("%s's main.nf lacks %q:\n%s", c.fixture, want, main)
			}
		}

		if strings.Contains(main, "us -> [k, fs, es, d, us + ds]") {
			t.Errorf("%s: an instance stages a whole output:\n%s", c.fixture, main)
		}
	}
}

// TestEmitSizesForkedGathers: a forked call's gather of its share of an
// inner map call is a groupKey of that share's fork count, from PLAN's
// counts for an entry-fed root (BY_ENTRY) or the producer's posted forks
// (BY_STAGE, by MKLIST), with an empty gather for a share with no fork.
// TestMrpDiff/map_nested_gather runs the fixture against mrp.
func TestEmitSizesForkedGathers(t *testing.T) {
	main := readTree(t, emitFixture(t, "map_nested_gather"))["main.nf"]

	for _, want := range []string{
		".combine(PLAN.out.n1.flatMap { s -> s.readLines().collect { l -> def f = l.tokenize(' '); [f.size() > 1 ? f[1].tokenize('.').collect { t -> t as int } : [], f[0] as int] } }, by: 0).map { _o, c, xs, m -> [groupKey(c, m), xs] }",
		".combine(o2_forks.map { kp, ds -> [[kp[0]], ds.count { e -> e.parent.name == 'MNG.BY_STAGE.SQ' }] }, by: 0).map { _o, c, xs, m -> [groupKey(c, m), xs] }",
		".filter { v -> v[2] == 0 }.map { v -> [v[1], []] })",
		`eval('cat \'counts/MNG.BY_ENTRY.SQ\''), emit: n1`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("map_nested_gather's main.nf lacks %q:\n%s", want, main)
		}
	}

	if strings.Contains(main, ".unique().mix(") {
		t.Errorf("a forked gather waits for all of its upstream:\n%s", main)
	}
}

// TestEmitWaitsForPreflights: a preflight signals once every instance of it
// is done, and each instance of a node it gates waits for that signal
// (modifiers_min: CHECK gates INNER's DOUBLE and TRIPLE).
func TestEmitWaitsForPreflights(t *testing.T) {
	main := readTree(t, emitFixture(t, "modifiers_min"))["main.nf"]

	for _, want := range []string{
		"d0 = N0_CHECK.out.outs.toList().map { _xs -> true }",
		"i1 = i1.combine(d0).map { k, fs, es, _d -> [k, fs, es] }",
		"i2 = i2.combine(d0).map { k, fs, es, _d -> [k, fs, es] }",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("modifiers_min's main.nf lacks %q:\n%s", want, main)
		}
	}

	if strings.Contains(main, "i0 = i0.combine(d") {
		t.Errorf("the preflight waits for itself:\n%s", main)
	}
}
