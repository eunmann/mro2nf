package emit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		// the keyed disable bind exists alongside the non-keyed one
		"process DISABLE_5_INNER__DBL_K {",
		// the disable flag is resolved per outer fork and branched run/skip
		"gk_DBL = pa_l.flatMap { x -> x }.join(DISABLE_5_INNER__DBL_K.out).branch",
		// skipped outer forks emit the null bundle keyed by their key
		`sk_DBL = gk_DBL.skip.map { row -> tuple(row[0], file("${projectDir}/nulls/5_INNER__DBL")) }`,
		// only enabled forks feed FORKBIND (disable bundle stripped off the row);
		// the fork stages the shared types + its (reused) bind spec, not all assets
		`FORK_5_INNER__DBL_K(gk_DBL.run.map { row -> row[0..<row.size() - 1] }, types, file("${projectDir}/_assets/bindspecs/BIND_5_INNER__DBL.json"))`,
		// skipped forks are mixed back so every outer key has a result
		"ch_DBL_l = MERGE_5_INNER__DBL_K.out.mix(sk_DBL).toList()",
		// forks are enumerated from forknames.json (object-store-safe), not listFiles;
		// .resolve() preserves the s3:// scheme that a "${d}/..." GString would drop
		`d.resolve('forknames.json')`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("pipe_INNER.nf missing keyed disable wiring %q", want)
		}
	}

	if strings.Contains(mod, "listFiles()") {
		t.Error("pipe_INNER.nf uses listFiles() (cannot enumerate an s3:// work dir)")
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
		"cpus { (join?.threads ?: 0) > 0 ? Math.max(1, Math.ceil(join.threads as double) as int) : 1 }",
		`memory { (join?.mem_gb ?: 0) > 0 ? "${join.mem_gb} GB" : '1 GB' }`,
		"val join",
		// the workflow parses joinres.json into the join val
		"join = SUM_SQUARES_SPLIT.out.joinres.map { f -> new groovy.json.JsonSlurper().parseText(f.text) }",
		"SUM_SQUARES_JOIN(join, a, SUM_SQUARES_SPLIT.out.defs, SUM_SQUARES_MAIN.out.collect(), types)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("stage_SUM_SQUARES.nf missing join-override wiring %q", want)
		}
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

// TestEmitNoSpecialOmitsClusterOptions checks that a stage without a `special`
// key emits no clusterOptions directive (no scheduler noise for the common case).
func TestEmitNoSpecialOmitsClusterOptions(t *testing.T) {
	dir := loadAndEmit(t)

	data, err := os.ReadFile(filepath.Join(dir, "modules", "stage_SUM_SQUARES.nf"))
	if err != nil {
		t.Fatalf("read stage module: %v", err)
	}

	if strings.Contains(string(data), "clusterOptions") {
		t.Error("stage with no special should not emit clusterOptions")
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
		`(params.reads ?: []).collect { __e -> (__e != null ? [file(__e)] : []) }.flatten()`,
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
// (preflight/local/volatile) rather than applying them silently, so a ported
// pipeline's behavior differences are visible at transpile time.
func TestWarnings(t *testing.T) {
	ast, err := frontend.Parse("../../testdata/modifiers_min/pipeline.mro", []string{"../../testdata/modifiers_min"}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prog, err := frontend.Lower(ast)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	warns := emit.Warnings(prog)

	var sawPreflight bool
	for _, w := range warns {
		if strings.Contains(w, "preflight") {
			sawPreflight = true
		}
	}

	if !sawPreflight {
		t.Errorf("expected a preflight no-op warning, got %v", warns)
	}

	// A pipeline with no modifiers must produce no warnings (no false noise).
	ast2, _ := frontend.Parse("../../testdata/split_test/pipeline.mro", []string{"../../testdata/split_test"}, false)
	prog2, _ := frontend.Lower(ast2)

	if w := emit.Warnings(prog2); len(w) != 0 {
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
			"include { wf_SUM_SQUARES as wf_19_SUM_SQUARE_PIPELINE__SUM_SQUARES }",
			// Bind outputs are value channels, so callee results feed multiple
			// consumers directly (no redundant, warning-triggering .first()).
			"ch_SUM_SQUARES = wf_19_SUM_SQUARE_PIPELINE__SUM_SQUARES(BIND_19_SUM_SQUARE_PIPELINE__SUM_SQUARES.out)",
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
			"cpus { (res?.threads",
			"memory { (res?.mem_gb",
			// Static using(mem_gb=2) maps to the split/join phase memory.
			"memory '2 GB'",
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
