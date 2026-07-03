package emit_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/google/go-cmp/cmp"
)

// emitFixture parses, lowers, and emits a testdata fixture into a temp dir,
// returning the output directory for assertions on the generated Nextflow.
func emitFixture(t *testing.T, fixture string, stageCode map[string]string) string {
	t.Helper()

	base := "../../testdata/" + fixture
	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	if err := emit.Emit(prog, emit.Options{
		OutDir:    dir,
		Mre:       "mre",
		Shell:     "/x/martian_shell.py",
		MROFile:   "pipeline.mro",
		MRODir:    base,
		StageCode: stageCode,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	return dir
}

// realRuntime creates existing on-disk runtime sources (mre binary, an adapter
// dir with martian_shell.py, and stage code dirs) for tests that emit a container
// target, which now verifies these sources exist before writing the Dockerfile.
func realRuntime(t *testing.T) (string, string, map[string]string) {
	t.Helper()

	root := t.TempDir()
	mre := filepath.Join(root, "mre")
	shell := filepath.Join(root, "adapters", "martian_shell.py")
	stages := map[string]string{"SUM_SQUARES": filepath.Join(root, "ss"), "REPORT": filepath.Join(root, "report")}

	if err := os.WriteFile(mre, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(shell), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shell, []byte("# adapter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range stages {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return mre, shell, stages
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func loadAndEmit(t *testing.T) string {
	t.Helper()

	return emitFixture(t, "split_test", map[string]string{
		"SUM_SQUARES": "/x/sum_squares",
		"REPORT":      "/x/report",
	})
}

func TestEmitFiles(t *testing.T) {
	dir := loadAndEmit(t)

	for _, rel := range []string{
		"main.nf",
		"nextflow.config",
		"lib/Mro2nf.groovy",
		"entry_args/data.json",
		"_assets/types.json",
		"modules/pipe_SUM_SQUARE_PIPELINE.nf",
		"modules/stage_SUM_SQUARES.nf",
		"modules/stage_REPORT.nf",
		"_assets/bindspecs/BIND_19_SUM_SQUARE_PIPELINE__SUM_SQUARES.json",
		"_assets/bindspecs/BIND_19_SUM_SQUARE_PIPELINE__REPORT.json",
		"_assets/bindspecs/BIND_19_SUM_SQUARE_PIPELINE__return.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected file %s: %v", rel, err)
		}
	}
}

// TestEmitConfig checks nextflow.config declares every executor profile and the
// auto-retry analog, so a -profile run resolves and transient failures retry.
func TestEmitConfig(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "nextflow.config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	cfg := string(data)
	for _, want := range []string{
		"params.outdir = 'results'",
		"standard { process.executor = 'local' }",
		"slurm    { process.executor = 'slurm' }",
		"sge      { process.executor = 'sge' }",
		"lsf      { process.executor = 'lsf' }",
		"pbs      { process.executor = 'pbs' }",
		"process.executor = 'awsbatch'",
		"k8s { process.executor = 'k8s' }",
		"task.exitStatus == 42 ? 'terminate' : 'retry'",
		"maxRetries = 2",
		"params.aws_queue = null",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config missing %q", want)
		}
	}
}

// TestEmitPublishConditional checks the PUBLISH process disables the publishDir
// copy when params.outdir is null (C2: awsbatch defaults to no launcher-local
// publish), so a null outdir never errors and never copies to the launcher.
func TestEmitPublishConditional(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "main.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "enabled: params.outdir != null") {
		t.Error("PUBLISH publishDir must be gated on params.outdir != null so a null outdir is a no-op")
	}
}

// TestEmitConfigTargets checks the per-target nextflow.config: awsbatch wires the
// Batch executor + classic S3 staging with a parameterized container, and
// healthomics publishes to the managed pubdir, pins a Nextflow version, and sets
// no executor (execution is managed).
func TestEmitConfigTargets(t *testing.T) {
	emitCfg := func(target emit.Target) string {
		t.Helper()

		ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		prog, err := frontend.Lower(ast)
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		dir := t.TempDir()
		// Container targets stage real runtime sources into the build context, so
		// provide existing files (a bare "mre" would now fail the existence check).
		mre, shell, stages := realRuntime(t)
		if err := emit.Emit(prog, emit.Options{
			OutDir: dir, Mre: mre, Shell: shell, MROFile: "pipeline.mro", Target: target,
			Container: "ecr/mro2nf:1", StageCode: stages,
		}); err != nil {
			t.Fatalf("emit: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "nextflow.config"))
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	batch := emitCfg(emit.TargetAWSBatch)
	for _, want := range []string{
		"params.container = 'ecr/mro2nf:1'",
		"container = params.container",
		"process.executor = 'awsbatch'",
		"aws.batch.cliPath = params.aws_cli_path",
		// C2: no launcher-local publish by default; the curated copy is opt-in.
		"params.aws_outdir = null",
		"params.outdir = params.aws_outdir",
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("awsbatch config missing %q", want)
		}
	}

	// The relative 'results' default must NOT appear on awsbatch — it would publish
	// to the ephemeral launcher rather than S3.
	if strings.Contains(batch, "params.outdir = 'results'") {
		t.Error("awsbatch config should not default params.outdir to the launcher-local 'results'")
	}

	if strings.Contains(batch, "profiles {") {
		t.Error("awsbatch config should set the executor directly, not bury it in a profile")
	}

	omics := emitCfg(emit.TargetHealthOmics)
	for _, want := range []string{
		"params.outdir = '/mnt/workflow/pubdir'",
		"container = params.container",
		"manifest.nextflowVersion",
	} {
		if !strings.Contains(omics, want) {
			t.Errorf("healthomics config missing %q", want)
		}
	}

	if strings.Contains(omics, "process.executor") {
		t.Error("healthomics manages execution; config must not set process.executor")
	}
}

// TestEmitMonitorAndContainer checks the -monitor and -container options reach
// the generated stage commands and config.
func TestEmitMonitorAndContainer(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	err = emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: "mre", Shell: "/x/martian_shell.py", MROFile: "pipeline.mro",
		Monitor: true, Container: "ecr/mre:latest",
		StageCode: map[string]string{"SUM_SQUARES": "/x/sum_squares", "REPORT": "/x/report"},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	mod, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(mod), " -monitor") {
		t.Error("stage command missing -monitor with Monitor: true")
	}

	cfg, err := os.ReadFile(filepath.Join(dir, "nextflow.config"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(cfg), "container = 'ecr/mre:latest'") {
		t.Error("nextflow.config missing process.container line")
	}
}

// TestEmitDisabledNestedMap checks the keyed (nested) disable gate: a map call
// that is BOTH disabled and itself nested inside an outer map must, per outer
// fork, resolve its disable flag and either run its inner map or emit the null
// bundle. The non-keyed path already had this gate; this asserts the keyed path
// wires it too (regression guard for the silently-dropped disable modifier).
func TestEmitDisabledNestedMap(t *testing.T) {
	dir := emitFixture(t, "map_pipe_disabled_nested", map[string]string{"DBL": "/x/dbl"})

	data, err := os.ReadFile(filepath.Join(dir, "modules", "pipe_INNER.nf"))
	if err != nil {
		t.Fatalf("read pipe_INNER.nf: %v", err)
	}

	mod := string(data)
	for _, want := range []string{
		// the disable flag (self.skip) is read natively per outer fork from the
		// per-fork args (row[1]) and branched run/skip — no DISABLE_K task (#59)
		"gk_DBL = pa_l.flatMap { x -> x }.branch { row ->",
		"def off = Mro2nf.disabledDir(row[1], 'skip')",
		// skipped outer forks emit the null bundle keyed by their key
		`sk_DBL = gk_DBL.skip.map { row -> tuple(row[0], file("${projectDir}/nulls/5_INNER__DBL")) }`,
		// enabled forks feed FORKBIND directly (no disable bundle to strip); the
		// fork stages the shared types + its (reused) bind spec, not all assets
		`FORK_5_INNER__DBL_K(gk_DBL.run, types, file("${projectDir}/_assets/bindspecs/BIND_5_INNER__DBL.json"))`,
		// skipped forks are mixed back so every outer key has a result
		"ch_DBL_l = MERGE_5_INNER__DBL_K.out.mix(sk_DBL).toList()",
		// forks are enumerated from forknames.json (object-store-safe, not
		// listFiles) via the shipped helper (#49)
		"Mro2nf.forkTuples(ok, d)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("pipe_INNER.nf missing keyed disable wiring %q", want)
		}
	}

	if strings.Contains(mod, "listFiles()") {
		t.Error("pipe_INNER.nf uses listFiles() (cannot enumerate an s3:// work dir)")
	}

	// The keyed disable is native now — no DISABLE_K bind, no join on its output.
	if strings.Contains(mod, "process DISABLE_5_INNER__DBL_K") {
		t.Error("a natively-gated keyed disable must emit no DISABLE_K process (#59)")
	}

	if strings.Contains(mod, "Mro2nf.disabled(row[-1])") {
		t.Error("keyed disable must read the flag natively (disabledDir), not from a DISABLE bundle")
	}
}

// TestEmitJoinResourceOverride checks the split-returned join override plumbing:
// SPLIT emits joinres.json, the workflow parses it into a `join` val, and JOIN
// provisions cpus/memory dynamically from it (falling back to the stage default)
// and passes the resolved allocation to the shim so its _jobinfo matches mrp.
func TestEmitJoinResourceOverride(t *testing.T) {
	dir := emitFixture(t, "join_resources", map[string]string{"SUM_SQUARES": "/x/sum_squares"})

	data, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatalf("read stage module: %v", err)
	}

	mod := string(data)
	for _, want := range []string{
		// the split emits the join override as a file output
		"path 'joinres.json', emit: joinres",
		"-joinres joinres.json",
		// the split's own _jobinfo is now accurate (passes its allocation)
		"-work . -o chunks.json -joinres joinres.json -chunkdir . -threads ${task.cpus} -memgb ${task.memory.toGiga()}",
		// JOIN provisions from the override, falling back to the stage default
		"cpus { def t = Math.abs((join?.threads ?: 0) as double); t > 0 ? Math.max(1, Math.ceil(t) as int) : 1 }",
		`memory { def m = Math.abs((join?.mem_gb ?: 0) as double); m = m > 0 ? m : 1; (m * task.attempt) + ' GB' }`,
		"val join",
		// the workflow parses joinres.json into the join val via the shipped helper (#49)
		"join = SUM_SQUARES_SPLIT.out.joinres.map { f -> Mro2nf.parseJson(f) }",
		"SUM_SQUARES_JOIN(join, a, SUM_SQUARES_SPLIT.out.defs, SUM_SQUARES_MAIN.out.collect().ifEmpty([]), types)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("stage_SUM_SQUARES.nf missing join-override wiring %q", want)
		}
	}
}

// TestEmitSplitJoinRunsWithZeroChunks guards bug 5: a split that produces 0
// chunks must still run its JOIN. Nextflow's collect() on an empty channel emits
// nothing, so the non-keyed JOIN wiring must guard it with .ifEmpty([]) — matching
// Martian, which writes _chunk_outs=[] and runs the join anyway (core/stage.go).
func TestEmitSplitJoinRunsWithZeroChunks(t *testing.T) {
	dir := emitFixture(t, "join_resources", map[string]string{"SUM_SQUARES": "/x/sum_squares"})

	data, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatalf("read stage module: %v", err)
	}

	want := "SUM_SQUARES_JOIN(join, a, SUM_SQUARES_SPLIT.out.defs, SUM_SQUARES_MAIN.out.collect().ifEmpty([]), types)"
	if !strings.Contains(string(data), want) {
		t.Errorf("non-keyed JOIN wiring must guard the empty channel; missing %q", want)
	}
}

// TestEmitSpecialScheduler checks that a stage's `using(special=...)` key is
// mapped to a clusterOptions directive on every phase, looked up from
// params.job_resources (mrp's MRO_JOBRESOURCES analog). A per-task __special
// (per-chunk for main, the split-returned override for join) wins over the
// static key; split/single phases use the static key directly.
func TestEmitSpecialScheduler(t *testing.T) {
	dir := emitFixture(t, "special_resource", map[string]string{"SUM_SQUARES": "/x/sum_squares"})

	cfg, err := os.ReadFile(filepath.Join(dir, "nextflow.config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if !strings.Contains(string(cfg), "params.job_resources = [:]") {
		t.Error("nextflow.config missing default params.job_resources map")
	}

	data, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatalf("read stage module: %v", err)
	}

	mod := string(data)
	for _, want := range []string{
		// split phase: the static special key
		"clusterOptions { params.job_resources?.get('highmem') ?: '' }",
		// main phase: per-chunk __special override, static fallback
		"clusterOptions { params.job_resources?.get(res?.special ?: 'highmem') ?: '' }",
		// join phase: split-returned join __special override, static fallback
		"clusterOptions { params.job_resources?.get(join?.special ?: 'highmem') ?: '' }",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("stage_SUM_SQUARES.nf missing special wiring %q", want)
		}
	}
}

// TestEmitNoSpecialClusterOptions checks the `special` routing for stages with no
// static key: a non-split stage (REPORT) emits no clusterOptions at all, while a
// split stage's main/join phases carry a dynamic router that resolves a
// split-returned __special (and is a no-op — resolves to ” — when none is
// returned). The split phase itself emits nothing.
func TestEmitNoSpecialClusterOptions(t *testing.T) {
	dir := loadAndEmit(t)

	report := readFile(t, filepath.Join(dir, "modules", "stage_REPORT.nf"))
	if strings.Contains(report, "clusterOptions") {
		t.Error("a non-split stage with no special must not emit clusterOptions")
	}

	sumsq := readFile(t, filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	// main and join route a split-returned __special even without a static key.
	for _, want := range []string{
		"clusterOptions { params.job_resources?.get(res?.special ?: '') ?: '' }",
		"clusterOptions { params.job_resources?.get(join?.special ?: '') ?: '' }",
	} {
		if !strings.Contains(sumsq, want) {
			t.Errorf("split stage main/join must route a split-returned __special: missing %q", want)
		}
	}
}

// TestEmitVmemFlag checks that a stage's static using(vmem_gb) is passed to the
// shim via -vmemgb on every phase (so --monitor caps at the declared value, not
// the memory-derived default): a plain value on split/main, and a join phase that
// lets a split-returned join __vmem_gb override refine it. A stage with no
// vmem_gb emits no -vmemgb.
func TestEmitVmemFlag(t *testing.T) {
	dir := emitFixture(t, "vmem_stage", map[string]string{"VSTAGE": "/x/vstage"})
	mod := readFile(t, filepath.Join(dir, "modules", "stage_VSTAGE.nf"))

	for _, want := range []string{
		"-vmemgb 8",
		"-vmemgb ${(join?.vmem_gb ?: 0) > 0 ? join.vmem_gb : 8}",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("stage_VSTAGE.nf missing vmem flag %q", want)
		}
	}

	// A stage with no using(vmem_gb) must not emit -vmemgb (no noise; the shim
	// derives vmem from memory).
	noVmem := readFile(t, filepath.Join(loadAndEmit(t), "modules", "stage_SUM_SQUARES.nf"))
	if strings.Contains(noVmem, "-vmemgb") {
		t.Error("a stage without vmem_gb must not emit -vmemgb")
	}
}

// TestEmitPreflightGate checks that an input-bound preflight call gates the rest
// of the pipeline: it is wired first against the raw pipeargs, then `pa` is
// reassigned to a channel that only emits once the preflight completes, so every
// downstream call waits for it (mrp's prenode dependency, via the shared pa).
func TestEmitPreflightGate(t *testing.T) {
	dir := emitFixture(t, "modifiers_min", map[string]string{
		"CHECK": "/x/check", "DOUBLE": "/x/double", "TRIPLE": "/x/triple",
	})

	mod := readFile(t, filepath.Join(dir, "modules", "pipe_TOP.nf"))
	check := strings.Index(mod, "ch_CHECK = ")
	gate := strings.Index(mod, "pa = ch_CHECK.combine(pipeargs).map { tuple(it[-2], it[-1]) }.first()")
	inner := strings.Index(mod, "BIND_3_TOP__INNER(")
	if check < 0 || gate < 0 || inner < 0 {
		t.Fatalf("preflight wiring missing: check=%d gate=%d inner=%d", check, gate, inner)
	}
	// order: preflight wired, then pa gated, then the gated downstream call.
	if check >= gate || gate >= inner {
		t.Errorf("preflight must be wired before the gate, and the gate before downstream calls (check=%d gate=%d inner=%d)", check, gate, inner)
	}
}

// TestEmitGPUAccelerator checks the reserved `special = "gpu[:N]"` key: it emits
// an `accelerator N` directive (not clusterOptions) on the compute phase only —
// a non-split stage's process and a split stage's MAIN, never SPLIT or JOIN.
func TestEmitGPUAccelerator(t *testing.T) {
	dir := emitFixture(t, "gpu_stage", map[string]string{"INFER": "/x/infer", "TRAIN": "/x/train"})

	infer := readFile(t, filepath.Join(dir, "modules", "stage_INFER.nf"))
	if !strings.Contains(infer, "accelerator 1") {
		t.Error("non-split GPU stage INFER must request `accelerator 1`")
	}
	if strings.Contains(infer, "clusterOptions") {
		t.Error("a gpu special must emit accelerator, not clusterOptions")
	}

	train := readFile(t, filepath.Join(dir, "modules", "stage_TRAIN.nf"))
	if !strings.Contains(train, "accelerator 2") {
		t.Error("split GPU stage TRAIN main must request `accelerator 2`")
	}
	// accelerator belongs only on the compute (MAIN) phase, not SPLIT/JOIN. TRAIN
	// is not map-called, so its keyed variants are pruned (#59) — exactly one
	// accelerator directive (plain MAIN), and no _MAIN_K process at all.
	if got := strings.Count(train, "accelerator"); got != 1 {
		t.Errorf("TRAIN has %d accelerator directives, want 1 (MAIN only, not SPLIT/JOIN)", got)
	}

	if strings.Contains(train, "TRAIN_MAIN_K") {
		t.Error("TRAIN is not map-called; its keyed _MAIN_K variant must be pruned (#59)")
	}
}

// TestEmitNativeDisableGate guards #59 Levers 2+3: a disabled call whose flag is
// a single top-level field reads it natively on the driver (Mro2nf.disabledField)
// with no DISABLE task; and a natively-disabled non-split leaf stage additionally
// fuses its bind into the stage task, so no standalone BIND either. modifiers_min
// gates the TRIPLE leaf stage on self.skip; disabled_callref gates the WORK leaf
// stage on an upstream output FLAG.on; disabled_map gates a map call (not fused).
func TestEmitNativeDisableGate(t *testing.T) {
	m := emitFixture(t, "modifiers_min", map[string]string{"DOUBLE": "/x/d", "TRIPLE": "/x/t", "CHECK": "/x/c"})

	top := readFile(t, filepath.Join(m, "modules", "pipe_TOP.nf"))
	// self.skip read natively from the fork args, and the enabled branch feeds the
	// FUSED bind+main stage directly (Lever 3) — no BIND, no DISABLE.
	if !strings.Contains(top, "Mro2nf.disabledField(data, 'skip')") {
		t.Errorf("self.skip disable must be read natively:\n%s", top)
	}
	if !strings.Contains(top, "r_TRIPLE = STAGE_3_TOP__TRIPLE(g_TRIPLE.run") {
		t.Errorf("a disabled leaf stage must fuse bind into the stage task:\n%s", top)
	}
	if strings.Contains(top, "process BIND_3_TOP__TRIPLE") {
		t.Errorf("a fused disabled leaf stage must emit no standalone BIND:\n%s", top)
	}
	if strings.Contains(top, "process DISABLE") {
		t.Errorf("a natively-gated disable must emit no DISABLE process:\n%s", top)
	}

	d := emitFixture(t, "disabled_callref", map[string]string{"FLAG": "/x/f", "WORK": "/x/w"})

	dp := readFile(t, filepath.Join(d, "modules", "pipe_DC.nf"))
	// FLAG.on read natively by combining pa with the producing channel; WORK (a
	// leaf stage) fuses too.
	if !strings.Contains(dp, "Mro2nf.disabledField(gd, 'on')") {
		t.Errorf("FLAG.on disable must be read natively from the upstream channel:\n%s", dp)
	}
	if !strings.Contains(dp, "r_WORK = STAGE_2_DC__WORK(") {
		t.Errorf("a disabled upstream-ref leaf stage must fuse bind into the stage task:\n%s", dp)
	}
	if strings.Contains(dp, "process DISABLE") {
		t.Errorf("a natively-gated upstream-ref disable must emit no DISABLE process:\n%s", dp)
	}

	// A disabled MAP call gates natively (reads the flag from pa) but is NOT fused
	// — it still needs the FORK for fan-out: disabled_map's `map call DBL using
	// (disabled = self.skip)`.
	dm := emitFixture(t, "disabled_map", map[string]string{"DBL": "/x/dbl"})

	dmp := readFile(t, filepath.Join(dm, "modules", "pipe_Q.nf"))
	if !strings.Contains(dmp, "Mro2nf.disabledField(data, 'skip')") {
		t.Errorf("a disabled map call must gate natively on pa:\n%s", dmp)
	}
	if strings.Contains(dmp, "process DISABLE") {
		t.Errorf("a natively-gated disabled map call must emit no DISABLE process:\n%s", dmp)
	}
}

// TestEmitFuseChains guards #59 Lever 4: -fuse-chains folds a single-consumer
// source stage (SRC) into its transform-consumer's task (USE), running both
// stages' bind+main inline in one process; without the flag they stay separate.
func TestEmitFuseChains(t *testing.T) {
	base := "../../testdata/chain_fuse"

	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	emitCH := func(fuse bool) string {
		dir := t.TempDir()
		if err := emit.Emit(prog, emit.Options{
			OutDir: dir, Mre: "mre", Shell: "/x/s", MROFile: "pipeline.mro", MRODir: base,
			StageCode:  map[string]string{"SRC": "/x/src", "USE": "/x/use"},
			FuseChains: fuse,
		}); err != nil {
			t.Fatalf("emit(fuse=%v): %v", fuse, err)
		}

		return readFile(t, filepath.Join(dir, "modules", "pipe_CH.nf"))
	}

	def := emitCH(false)
	if !strings.Contains(def, "process STAGE_2_CH__SRC") || strings.Contains(def, "spec_prod.json") {
		t.Errorf("default must keep SRC standalone and not fuse the chain:\n%s", def)
	}

	on := emitCH(true)
	if strings.Contains(on, "process STAGE_2_CH__SRC") {
		t.Errorf("-fuse-chains must fold the SRC producer into USE:\n%s", on)
	}
	if !strings.Contains(on, "-inputs SRC=outs_0") {
		t.Errorf("-fuse-chains must feed the producer's outputs into the consumer bind:\n%s", on)
	}
}

// TestEmitPrunesUnusedKeyedVariants guards #59: a stage/pipeline that never runs
// under a map call gets no fork-keyed variant emitted, while a map-reachable one
// keeps it. Behavior is unchanged (the pruned processes were never invoked) —
// this is verified byte-identical by the e2e/differential suites; here we pin the
// emitted structure.
func TestEmitPrunesUnusedKeyedVariants(t *testing.T) {
	// diamond_min has no map call anywhere, so no keyed layer at all.
	d := emitFixture(t, "diamond_min", map[string]string{"GEN": "/x/gen", "ADD": "/x/add"})

	gen := readFile(t, filepath.Join(d, "modules", "stage_GEN.nf"))
	if strings.Contains(gen, "GEN_MAP") || strings.Contains(gen, "wf_GEN_map") {
		t.Error("GEN is never map-called; its keyed variant must be pruned (#59)")
	}

	pipeD := readFile(t, filepath.Join(d, "modules", "pipe_D.nf"))
	if strings.Contains(pipeD, "wf_D_map") || strings.Contains(pipeD, "wfk_") {
		t.Error("pipeline D is never map-called; its keyed layer must be pruned (#59)")
	}

	// map_pipe maps sub-pipeline INNER over an array, so INNER and its stage ADD1
	// run keyed and MUST keep their variants; the top-level OUTER does not.
	m := emitFixture(t, "map_pipe", map[string]string{"ADD1": "/x/add1"})

	if add1 := readFile(t, filepath.Join(m, "modules", "stage_ADD1.nf")); !strings.Contains(add1, "ADD1_MAP") {
		t.Error("ADD1 runs under a map call; its keyed variant must be emitted")
	}

	if inner := readFile(t, filepath.Join(m, "modules", "pipe_INNER.nf")); !strings.Contains(inner, "wf_INNER_map") {
		t.Error("INNER is map-called; its keyed variant must be emitted")
	}

	if outer := readFile(t, filepath.Join(m, "modules", "pipe_OUTER.nf")); strings.Contains(outer, "wf_OUTER_map") {
		t.Error("OUTER is the top-level pipeline (not map-called); its keyed layer must be pruned (#59)")
	}
}

// TestEmitAssetsStaged checks the cloud-portability fix: the type manifest and a
// process's own bindspec are staged into its task as individual `path` inputs
// (not referenced by ${projectDir}, which is invisible to an isolated AWS Batch /
// HealthOmics worker), and a task stages only its own bindspec rather than the
// whole _assets dir (C3). The head-node file() resolution still uses ${projectDir}.
func TestEmitAssetsStaged(t *testing.T) {
	dir := loadAndEmit(t)

	for _, rel := range []string{"modules/stage_SUM_SQUARES.nf", "modules/pipe_SUM_SQUARE_PIPELINE.nf", "main.nf"} {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		mod := string(data)

		// Commands must read the staged copies, never ${projectDir}; and no task may
		// stage the whole _assets dir (that would pull every call's bindspec).
		if strings.Contains(mod, "${projectDir}/types.json") || strings.Contains(mod, "${projectDir}/bindspecs") {
			t.Errorf("%s references types.json/bindspecs via ${projectDir} (not staged on isolated workers)", rel)
		}

		if strings.Contains(mod, "path '_assets'") {
			t.Errorf("%s stages the whole _assets dir; it must stage only the files it needs (C3)", rel)
		}
	}

	stage, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatal(err)
	}

	mod := string(stage)
	for _, want := range []string{
		"path 'types.json'",   // the shared manifest staged into the task
		"-types 'types.json'", // command reads the staged copy
		`types = file("${projectDir}/_assets/types.json")`, // workflow resolves it on the head node
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("stage_SUM_SQUARES.nf missing type-manifest staging %q", want)
		}
	}

	// A bind process stages its own bindspec as spec.json (not the whole dir).
	pipe, err := os.ReadFile(filepath.Join(dir, "modules", "pipe_SUM_SQUARE_PIPELINE.nf"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"path 'spec.json'",       // the per-process bindspec input
		"bind -spec 'spec.json'", // command reads the staged single spec
		"/_assets/bindspecs/",    // head-node resolves the specific spec file
	} {
		if !strings.Contains(string(pipe), want) {
			t.Errorf("pipe module missing per-bind spec staging %q", want)
		}
	}
}

// TestEmitObjectStoreSafe is the regression guard for the live-only S3 bug: a
// head-node closure that builds a path with file("${workdirPath}/sub") drops the
// s3:// scheme, so every such read must use Path.resolve() instead. The only
// allowed file("${...}") is on ${projectDir} (a local head-node path). This
// catches a future edit reintroducing the GString form — which passes locally
// and under docker_iso (lossless local interpolation) but breaks on a real S3
// work dir.
func TestEmitObjectStoreSafe(t *testing.T) {
	for _, fx := range []struct{ name, code string }{
		{"split_test", "SUM_SQUARES"},
		{"disabled_map", "DBL"},
		{"map_pipe_nested", "DBL"},
		{"join_resources", "SUM_SQUARES"},
	} {
		dir := emitFixture(t, fx.name, map[string]string{fx.code: "/x/" + fx.code})

		modules, _ := filepath.Glob(filepath.Join(dir, "modules", "*.nf"))
		modules = append(modules, filepath.Join(dir, "main.nf"))

		for _, m := range modules {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}

			// Find every `file("${` and ensure the interpolated head is projectDir.
			for _, frag := range strings.Split(string(data), `file("${`)[1:] {
				if !strings.HasPrefix(frag, "projectDir") {
					t.Errorf("%s: %s contains an object-store-unsafe file(\"${...}\") read (use Path.resolve()): ...%.40s", fx.name, filepath.Base(m), frag)
				}
			}
		}
	}
}

// TestEmitContainerMrjob checks that when a pipeline has comp stages (Mrjob set)
// the generated Dockerfile bakes the mrjob wrapper into the image — otherwise a
// comp stage's `-mrjob /opt/mro2nf/mrjob` reference would be missing on the worker.
func TestEmitContainerMrjob(t *testing.T) {
	tmp := t.TempDir()
	for _, n := range []string{"mre", "mrjob"} {
		if err := os.WriteFile(filepath.Join(tmp, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	ss, _ := filepath.Abs("../../testdata/split_test/stages/sum_squares")
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: filepath.Join(tmp, "mre"), Mrjob: filepath.Join(tmp, "mrjob"),
		Shell: ss + "/__init__.py", MROFile: "pipeline.mro", Target: emit.TargetAWSBatch,
		StageCode: map[string]string{"SUM_SQUARES": ss, "REPORT": ss},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	df, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(df), "COPY runtime/mrjob /opt/mro2nf/mrjob") {
		t.Error("Dockerfile must COPY mrjob when the pipeline has comp stages (Mrjob set)")
	}

	if _, err := os.Stat(filepath.Join(dir, "runtime", "mrjob")); err != nil {
		t.Errorf("build context missing runtime/mrjob: %v", err)
	}
}

// TestEmitCompRequiresMrjob checks that a program with a comp-adapter stage
// fails the transpile when no -mrjob is supplied (the generated project could
// only fail at run time), and emits normally once one is.
func TestEmitCompRequiresMrjob(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/comp_split/pipeline.mro", []string{"../../testdata/comp_split"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	opts := emit.Options{
		OutDir: t.TempDir(), Mre: "mre", Shell: "/x/martian_shell.py",
		MROFile: "pipeline.mro", StageCode: map[string]string{"COMPSUM": "/x/compsum"},
	}

	err = emit.Emit(prog, opts)
	if !errors.Is(err, apperror.ErrUnsupported) {
		t.Fatalf("Emit without -mrjob: want ErrUnsupported, got %v", err)
	}

	opts.Mrjob = "/x/mrjob.sh"
	if err := emit.Emit(prog, opts); err != nil {
		t.Fatalf("Emit with -mrjob: %v", err)
	}
}

// TestEmitHealthOmicsPackaging checks the HealthOmics target emits the workflow
// packaging artifacts: a parameter-template.json prompting for the ECR container
// URI, and a package.sh that zips the workflow while excluding the Docker build
// context (which ships as the image, not the workflow definition).
func TestEmitHealthOmicsPackaging(t *testing.T) {
	mre := filepath.Join(t.TempDir(), "mre")
	if err := os.WriteFile(mre, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: mre, MROFile: "pipeline.mro", Target: emit.TargetHealthOmics,
		StageCode: map[string]string{"SUM_SQUARES": mre, "REPORT": mre},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	tmpl, err := os.ReadFile(filepath.Join(dir, "parameter-template.json"))
	if err != nil {
		t.Fatalf("read parameter-template.json: %v", err)
	}

	var got map[string]map[string]any
	if err := json.Unmarshal(tmpl, &got); err != nil {
		t.Fatalf("parameter-template.json is not valid JSON: %v", err)
	}

	if _, ok := got["container"]; !ok {
		t.Error("parameter-template.json must prompt for the ECR container URI")
	}

	pkg, err := os.ReadFile(filepath.Join(dir, "package.sh"))
	if err != nil {
		t.Fatalf("read package.sh: %v", err)
	}

	for _, want := range []string{"zip -r workflow.zip", "-x 'runtime/*'", "-x 'Dockerfile'"} {
		if !strings.Contains(string(pkg), want) {
			t.Errorf("package.sh missing %q", want)
		}
	}
}

// TestEmitContainerBuild checks that a container target assembles a self-contained
// Docker build context (mre + adapters + stage code under runtime/), emits a
// Dockerfile, and bakes in-container /opt/mro2nf paths into the scripts — never the
// host paths, which don't exist inside the image.
func TestEmitContainerBuild(t *testing.T) {
	mre := filepath.Join(t.TempDir(), "mre")
	if err := os.WriteFile(mre, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ast, err := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	ssDir, _ := filepath.Abs("../../testdata/split_test/stages/sum_squares")
	shell, _ := filepath.Abs("../../vendor-martian/python/martian_shell.py")
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: mre, Shell: shell, MROFile: "pipeline.mro",
		Target: emit.TargetAWSBatch, Container: "ecr/mro2nf:1",
		StageCode: map[string]string{"SUM_SQUARES": ssDir, "REPORT": ssDir},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	df, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	for _, want := range []string{"FROM --platform=linux/amd64", "COPY runtime/mre /opt/mro2nf/mre", "awscli"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}

	// An ENTRYPOINT *instruction* starts a line; the explanatory comment ("No
	// ENTRYPOINT: ...") does not, so anchor on a leading newline.
	if strings.Contains("\n"+string(df), "\nENTRYPOINT") {
		t.Error("Dockerfile must not set an ENTRYPOINT (Batch/HealthOmics inject a bash launcher)")
	}

	for _, rel := range []string{"runtime/mre", "runtime/adapters/martian_shell.py", "runtime/stages/SUM_SQUARES"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("build context missing %s: %v", rel, err)
		}
	}

	mod, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(mod), "'/opt/mro2nf/mre'") || !strings.Contains(string(mod), "-stagecode '/opt/mro2nf/stages/SUM_SQUARES'") {
		t.Error("scripts must bake in-container /opt/mro2nf paths for a container target")
	}

	if strings.Contains(string(mod), ssDir) {
		t.Error("scripts must not bake host stage-code paths for a container target")
	}
}

// TestEmitEntryParams checks entry-input parameterization: each entry input is a
// nullable run param and BUILD_ENTRY_ARGS overlays the supplied values on the
// baked defaults at launch (so inputs can be set via -params-file / HealthOmics
// run params without re-transpiling).
func TestEmitEntryParams(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "main.nf"))
	if err != nil {
		t.Fatalf("read main.nf: %v", err)
	}

	main := string(data)
	for _, want := range []string{
		"params.values = null",       // the entry input is overridable
		"process BUILD_ENTRY_ARGS {", // builds the bundle at run time
		"values = groovy.json.JsonOutput.toJson([values: params.values])",  // overrides from params
		"entryargs -base entry_args -values values.json -o entry_resolved", // overlay on baked defaults
		"pipeargs = BUILD_ENTRY_ARGS.out.first()",                          // feed the entry pipeline
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf missing entry-param wiring %q", want)
		}
	}
}

// TestEmitEntryFileParam checks file-typed entry-input staging (C1): a scalar
// file input's leaves are flattened, staged through Nextflow as a `path` input,
// and reconstructed by entryargs via -fileflat, with the empty sentinel feeding
// the unset case.
func TestEmitEntryFileParam(t *testing.T) {
	dir := emitFixture(t, "entry_file", map[string]string{"COUNT": "/x/count"})

	data, err := os.ReadFile(filepath.Join(dir, "main.nf"))
	if err != nil {
		t.Fatalf("read main.nf: %v", err)
	}

	main := string(data)
	for _, want := range []string{
		"params.reads = null", // the file input is overridable
		// its file leaves staged as a list path input, each in a per-index subdir
		// so same-basename leaves do not collide in the task work dir
		"path(inflat_reads, stageAs: 'inflat_reads_?/*')",
		`[file(params.reads)]`,                            // flattened + file()'d on the head node
		`?: [file("${projectDir}/_assets/.entry_empty")]`, // unset / no-leaf falls back to the sentinel
		`-fileflat 'reads=${(inflat_reads instanceof List ? inflat_reads : [inflat_reads]).join(",")}'`, // staged paths reach entryargs
		`BUILD_ENTRY_ARGS(file("${projectDir}/entry_args"), values, types, flat_reads)`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf missing entry-file staging %q", want)
		}
	}

	// The sentinel file must exist (fed to BUILD_ENTRY_ARGS when the input is unset).
	if _, err := os.Stat(filepath.Join(dir, "_assets", ".entry_empty")); err != nil {
		t.Errorf("missing entry sentinel _assets/.entry_empty: %v", err)
	}

	// The non-file input flows through the values map, not as a staged path input.
	if strings.Contains(main, "inflat_scale") {
		t.Error("scale is not file-bearing; it must not be staged as a path input")
	}
}

// TestEmitEntryFileArray checks file-array (file[]) entry-input staging: the
// elements are flattened to a list, staged, and reconstructed in index order.
func TestEmitEntryFileArray(t *testing.T) {
	dir := emitFixture(t, "entry_filearr", map[string]string{"COUNTARR": "/x/countarr"})

	main := readFile(t, filepath.Join(dir, "main.nf"))
	for _, want := range []string{
		"path(inflat_reads, stageAs: 'inflat_reads_?/*')",
		`(params.reads ?: []).collect { __e0 -> (__e0 != null ? [file(__e0)] : []) }.flatten()`,
		`-fileflat 'reads=${(inflat_reads instanceof List ? inflat_reads : [inflat_reads]).join(",")}'`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf missing file-array staging %q", want)
		}
	}
}

// TestEmitEntryStructFile checks struct-with-file entry-input staging: the
// struct's file field is flattened (in field order) while scalar fields are not.
func TestEmitEntryStructFile(t *testing.T) {
	dir := emitFixture(t, "entry_struct_file", map[string]string{"READCFG": "/x/readcfg"})

	main := readFile(t, filepath.Join(dir, "main.nf"))
	// The struct param 'cfg' descends to its file field 'ref'; the int field 'n'
	// contributes nothing to the flattened file list.
	for _, want := range []string{
		"path(inflat_cfg, stageAs: 'inflat_cfg_?/*')",
		`)?.ref != null ? [file(`,
		`-fileflat 'cfg=${(inflat_cfg instanceof List ? inflat_cfg : [inflat_cfg]).join(",")}'`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.nf missing struct-file staging %q", want)
		}
	}
}

// TestWarnings checks the transpiler surfaces documented no-op divergences
// (local/volatile) rather than applying them silently, and — now that an
// input-bound preflight actually gates the pipeline — does NOT warn about it.
func TestWarnings(t *testing.T) {
	warnsFor := func(fx string) []string {
		t.Helper()
		ast, err := frontend.Parse("../../testdata/"+fx+"/pipeline.mro", []string{"../../testdata/" + fx}, false)
		if err != nil {
			t.Fatalf("parse %s: %v", fx, err)
		}
		prog, err := frontend.Lower(ast)
		if err != nil {
			t.Fatalf("lower %s: %v", fx, err)
		}

		return emit.Warnings(prog)
	}

	contains := func(ws []string, sub string) bool {
		for _, w := range ws {
			if strings.Contains(w, sub) {
				return true
			}
		}

		return false
	}

	// modifiers_min's preflight binds pipeline inputs, so it now gates the run —
	// no preflight warning. Its only other modifier is a disabled call (handled,
	// not a divergence), so it produces no warnings at all.
	if w := warnsFor("modifiers_min"); len(w) != 0 {
		t.Errorf("modifiers_min: an input-bound preflight gates and should not warn; got %v", w)
	}

	// kitchen_sink still has genuine no-op modifiers (local, volatile) that must
	// be surfaced.
	ks := warnsFor("kitchen_sink")
	if !contains(ks, "local") || !contains(ks, "volatile") {
		t.Errorf("kitchen_sink should warn on local and volatile no-ops, got %v", ks)
	}

	// A pipeline with no modifiers must produce no warnings (no false noise).
	if w := warnsFor("split_test"); len(w) != 0 {
		t.Errorf("split_test should have no warnings, got %v", w)
	}
}

func TestEmitEntryArgs(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "entry_args", "data.json"))
	if err != nil {
		t.Fatalf("read entry args: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse entry args: %v", err)
	}

	want := map[string]any{"values": []any{float64(1), float64(2), float64(3)}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entry args mismatch (-want +got):\n%s", diff)
	}
}

func TestEmitModules(t *testing.T) {
	dir := loadAndEmit(t)

	checks := map[string][]string{
		"main.nf": {
			"include { SUM_SQUARE_PIPELINE } from './modules/pipe_SUM_SQUARE_PIPELINE.nf'",
			"SUM_SQUARE_PIPELINE(pipeargs)",
		},
		"modules/pipe_SUM_SQUARE_PIPELINE.nf": {
			"workflow SUM_SQUARE_PIPELINE {",
			// SUM_SQUARES' bind (values=self.values) is a real transform on a split
			// stage, so #16 folds it into a per-call fused workflow that imports the
			// stage's MAIN/JOIN aliased and invokes them off a bind+split process —
			// no standalone BIND_SUM_SQUARES.
			"include { SUM_SQUARES_MAIN as STAGE_19_SUM_SQUARE_PIPELINE__SUM_SQUARES_MN; SUM_SQUARES_JOIN as STAGE_19_SUM_SQUARE_PIPELINE__SUM_SQUARES_JN }",
			"workflow STAGE_19_SUM_SQUARE_PIPELINE__SUM_SQUARES {",
			"ch_SUM_SQUARES = STAGE_19_SUM_SQUARE_PIPELINE__SUM_SQUARES(pa)",
		},
		"modules/stage_SUM_SQUARES.nf": {
			"process SUM_SQUARES_SPLIT {",
			"process SUM_SQUARES_MAIN {",
			"process SUM_SQUARES_JOIN {",
			"workflow wf_SUM_SQUARES {",
			// Paths are single-quoted so spaces/metacharacters are safe.
			"-stagecode '/x/sum_squares'",
			// Per-chunk resources reach the scheduler via dynamic directives
			// reading the chunk's resolved resources carried as a val.
			"cpus { def t = Math.abs((res?.threads",
			"memory { def m = Math.abs((res?.mem_gb",
			// Static using(mem_gb=2) maps to the split/join phase memory, which
			// grows with task.attempt (the --auto-adjust-memory analog).
			"memory { 2 * task.attempt + ' GB' }",
		},
		"modules/stage_REPORT.nf": {
			"process REPORT {",
			// using(threads=0.5) rounds up to one whole CPU.
			"cpus 1",
		},
	}

	for rel, wants := range checks {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)

			continue
		}

		for _, want := range wants {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s missing %q", rel, want)
			}
		}
	}
}
