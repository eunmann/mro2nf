package cgnf

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestEmitTargetsAsProfiles: one project runs on every target, chosen at
// launch with -profile (#309): every profile finds mre on PATH, the host's
// for the local and grid profiles and the image's for the container
// profiles, which name no tool path of their own, and every task of a
// container profile, PLAN and OUTS included, runs in the one image
// params.mro2nf_image names; a container profile without it fails at once.
// A stage task finds the code at the paths the project was transpiled from,
// PLAN and OUTS the project's copy of the .mro files.
func TestEmitTargetsAsProfiles(t *testing.T) {
	tree := readTree(t, emitFixture(t, "diamond_min"))
	cfg, ctr := tree["nextflow.config"], tree["conf/container.config"]

	checkContains(t, tree, "conf/base.config",
		"params.mro2nf_mre = 'mre'\n",
		"params.mro2nf_image = null\nparams.mro2nf_require_image = false\n",
	)
	checkContains(t, tree, "nextflow.config",
		"    docker {\n        process.executor = 'local'\n",
		"        docker.enabled = true\n        process.containerOptions = ",
		"'\n        includeConfig 'conf/container.config'\n",
		"    awsbatch {\n        process.executor = 'awsbatch'\n        includeConfig 'conf/container.config'\n        params.mro2nf_host_inputs = false\n",
		"    healthomics {\n        includeConfig 'conf/healthomics.config'\n",
	)

	if top, _, _ := strings.Cut(cfg, "profiles {"); !strings.Contains(top, "\nincludeConfig 'conf/base.config'\n") || strings.Contains("\n"+cfg, "\nparams.") {
		t.Errorf("nextflow.config does not include conf/base.config before its profiles, or sets a param outside them, which would override theirs:\n%s", cfg)
	}

	checkContains(t, tree, "conf/container.config",
		"params.mro2nf_confine = true\n",
		"params.mro2nf_require_image = true\n",
		"process.container = params.mro2nf_image\n",
	)

	if strings.Contains(ctr, "withName") {
		t.Errorf("conf/container.config gives a task an image of its own:\n%s", ctr)
	}

	for _, tool := range []string{"params.mro2nf_mre", "params.mro2nf_mrjob", "params.mro2nf_adapters"} {
		if strings.Contains(ctr, tool) {
			t.Errorf("conf/container.config names %s, where the image's PATH holds mre:\n%s", tool, ctr)
		}
	}

	checkContains(t, tree, "conf/healthomics.config", "includeConfig 'container.config'\n", "params.mro2nf_host_inputs = false\n", "params.outdir = '/mnt/workflow/pubdir'\n")

	main := tree["main.nf"]
	if !strings.Contains(main, "if (params.mro2nf_require_image && !params.mro2nf_image) {") || strings.Index(main, "!params.mro2nf_image") > strings.Index(main, "PLAN(") {
		t.Errorf("main.nf does not refuse a container profile without an image before PLAN:\n%s", main)
	}

	for _, want := range []string{"stage -phase ${phase} ${graph(meta, '')}", "outs ${graph(meta, 'mro')}"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// checkContains fails t for each of wants the tree's file name lacks.
func checkContains(t *testing.T, tree map[string]string, name string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(tree[name], want) {
			t.Errorf("%s lacks %q:\n%s", name, want, tree[name])
		}
	}
}

// TestEmitHealthOmicsRoundsMemoryUp: HealthOmics gives a task whole GiB,
// rounding down and refusing 0, so only its profile asks for mrp's request
// rounded up to a whole GiB, at least 1, through a selector that outranks the
// stage modules' memory directive. AWS Batch and the grid take it exactly.
func TestEmitHealthOmicsRoundsMemoryUp(t *testing.T) {
	tree := readTree(t, emitFixture(t, "diamond_min"))

	rule := `withName: '.*' {
        memory = { meta.mem_gb == null ? null : "${Math.max(1L, Math.ceil(meta.mem_gb as double) as long)} GB" }`
	if !strings.Contains(tree["conf/healthomics.config"], rule) {
		t.Errorf("conf/healthomics.config lacks the round-up rule %q:\n%s", rule, tree["conf/healthomics.config"])
	}

	for _, f := range []string{"nextflow.config", "conf/container.config"} {
		if strings.Contains(tree[f], "Math.ceil") {
			t.Errorf("%s rounds memory up, which reaches profiles other than healthomics:\n%s", f, tree[f])
		}
	}
}

// TestEmitMountsS3InputsOnAWSBatch: only the awsbatch profile mounts the
// input buckets, by default at the /mnt/s3 the example stack's instances
// mount them under, into every container, read-only, at the root the param
// names, and names that root to PLAN and every stage task, which take it
// only when the param is set, PLAN then uncached; elsewhere it is null and
// s3:// inputs are staged.
func TestEmitMountsS3InputsOnAWSBatch(t *testing.T) {
	tree := readTree(t, emitFixture(t, "entry_file"))
	cfg, base := tree["nextflow.config"], tree["conf/base.config"]

	checkContains(t, tree, "conf/base.config", "params.mro2nf_s3_mount = null\n")

	if strings.Count(base, "mro2nf_s3_mount = ") != 1 || strings.Contains(base, "aws.batch.volumes") {
		t.Errorf("conf/base.config mounts S3:\n%s", base)
	}

	start := strings.Index(cfg, "    awsbatch {\n")
	end := start + strings.Index(cfg[start:], "\n    }\n")

	if start < 0 || end < start {
		t.Fatalf("nextflow.config has no awsbatch profile:\n%s", cfg)
	}

	for _, want := range []string{"        params.mro2nf_s3_mount = '/mnt/s3'\n", "        aws.batch.volumes = \"${params.mro2nf_s3_mount}:${params.mro2nf_s3_mount}:ro\"\n"} {
		if !strings.Contains(cfg[start:end], want) {
			t.Errorf("the awsbatch profile lacks %q:\n%s", want, cfg[start:end])
		}
	}

	if strings.Count(cfg, "aws.batch.volumes") != 1 || strings.Count(cfg, "mro2nf_s3_mount = ") != 1 {
		t.Errorf("a profile other than awsbatch mounts S3:\n%s", cfg)
	}

	for _, f := range []string{"conf/container.config", "conf/healthomics.config"} {
		if strings.Contains(tree[f], "mro2nf_s3_mount") || strings.Contains(tree[f], "aws.batch.volumes") {
			t.Errorf("%s mounts S3:\n%s", f, tree[f])
		}
	}

	if want := "if (run.s3_mount) {\n        cmd += \" -s3-mount ${shq(run.s3_mount)}\""; !strings.Contains(stageModule, want) {
		t.Errorf("stage.nf lacks %q", want)
	}

	for _, want := range []string{"process PLAN {\n    cache !params.mro2nf_s3_mount\n", "${meta.run.s3_mount ? ' -s3-mount ' + shq(meta.run.s3_mount) : ''}", planRun, stageRun} {
		if !strings.Contains(tree["main.nf"], want) {
			t.Errorf("PLAN lacks %q:\n%s", want, tree["main.nf"])
		}
	}
}

// TestEmitHostInputsInPlaceOffContainers (#543): the local and grid
// profiles read a head-node entry input in place, and the container
// profiles, which all include conf/container.config, stage it; PLAN and
// every stage task take the one param, and PLAN the run's work dir, which no
// input read in place may hold.
func TestEmitHostInputsInPlaceOffContainers(t *testing.T) {
	tree := readTree(t, emitFixture(t, "entry_file"))
	cfg, base, ctr := tree["nextflow.config"], tree["conf/base.config"], tree["conf/container.config"]

	if strings.Contains(cfg, "mro2nf_host_in_place") || strings.Count(base, "mro2nf_host_in_place") != 1 || !strings.Contains(base, "params.mro2nf_host_in_place = true\n") {
		t.Errorf("conf/base.config does not read head-node inputs in place, or a profile other than the container ones sets it:\n%s\n%s", base, cfg)
	}

	if !strings.Contains(ctr, "params.mro2nf_host_in_place = false\n") || strings.Contains(tree["conf/healthomics.config"], "mro2nf_host_in_place") {
		t.Errorf("conf/container.config does not stage head-node inputs, alone:\n%s", ctr)
	}

	for _, want := range []string{" -host-in-place=${meta.run.host_in_place} ", " -run-workdir ${shq(workdir)}", ", workflow.workDir.toUriString(), "} {
		if !strings.Contains(tree["main.nf"], want) {
			t.Errorf("PLAN lacks %q:\n%s", want, tree["main.nf"])
		}
	}

	for _, run := range []string{planRun, stageRun} {
		if !strings.Contains(run, "host_in_place: params.mro2nf_host_in_place,") {
			t.Errorf("%s does not carry params.mro2nf_host_in_place", run)
		}
	}

	if want := "if (run.host_in_place) {\n        cmd += ' -host-in-place'"; !strings.Contains(stageModule, want) {
		t.Errorf("stage.nf lacks %q", want)
	}
}

// TestProcessesReadNoPathGlobal: on Nextflow's v2 parser, the 26.04 floor's
// default, a task's -resume key holds no global its process reads, so a
// changed value, such as another run's work dir, would silently reuse a
// stale task. Each process takes such a path as an input value. The v1
// parser, an opt-in the project does not support, keys every global and a
// Path global on its modification time, which each run changes for the
// work dir, so a process reading one would never resume.
func TestProcessesReadNoPathGlobal(t *testing.T) {
	tree := readTree(t, emitFixture(t, "entry_file"))

	for file, src := range map[string]string{"main.nf": tree["main.nf"], "stage.nf": stageModule} {
		procs := blocks(src, "process ")
		if len(procs) == 0 {
			t.Errorf("%s declares no process", file)
		}

		for _, body := range procs {
			for _, global := range []string{"workflow.", "projectDir", "launchDir", "moduleDir", "baseDir"} {
				if strings.Contains(body, global) {
					t.Errorf("%s: process %s reads %s", file, strings.Fields(body)[0], global)
				}
			}
		}
	}
}

var (
	// paramRead is a read of a launch param: params. after no identifier
	// character and no quote, which opens a message naming a param.
	paramRead = regexp.MustCompile(`(^|[^'\w])params\.`)
	runRead   = regexp.MustCompile(`\brun\.(\w+)`)
	runKey    = regexp.MustCompile(`(\w+): params\.`)
)

// TestScriptsReadParamsFromMeta (#576): a task's -resume key holds its
// inputs, but Nextflow 26.04 keys it on no param read by a function its
// script calls, and 26.04.0-26.04.1 on none its script reads
// (nextflow-io/nextflow#7156), so a changed param would silently reuse a
// stale task. So no process script reads params.,
// nor any function of the stage module, which the scripts call: each reads
// the run entry of its meta, which main.nf builds from the params, PLAN's
// from planRun and every other task's from stageRun, and which carries
// every key they read. A process's script is its block from its script:,
// shell: or exec: label on, a process's last section; the directives and
// inputs above it are not script.
func TestScriptsReadParamsFromMeta(t *testing.T) {
	main := readTree(t, emitFixture(t, "entry_file"))["main.nf"]

	for _, c := range []struct{ file, src, run string }{
		{"main.nf", main, planRun},
		{"stage.nf", stageModule, stageRun},
	} {
		if !strings.Contains(main, c.run) {
			t.Errorf("main.nf passes no %s", c.run)
		}

		carried := map[string]bool{}
		for _, m := range runKey.FindAllStringSubmatch(c.run, -1) {
			carried[m[1]] = true
		}

		code := scripts(t, c.file, c.src)
		for _, fn := range blocks(c.src, "def ") {
			code[strings.Fields(fn)[0]] = fn
		}

		for name, body := range code {
			if paramRead.MatchString(body) {
				t.Errorf("%s: %s reads a param, not its meta's run entry:\n%s", c.file, name, body)
			}

			for _, m := range runRead.FindAllStringSubmatch(body, -1) {
				if !carried[m[1]] {
					t.Errorf("%s: %s reads run.%s, which %s does not carry", c.file, name, m[1], c.run)
				}
			}
		}
	}
}

// blocks is the body of each top-level block of src that opens with
// keyword, from its name to its closing brace.
func blocks(src, keyword string) []string {
	parts := strings.Split(src, "\n"+keyword)[1:]
	out := make([]string, 0, len(parts))

	for _, block := range parts {
		body, _, _ := strings.Cut(block, "\n}\n")
		out = append(out, body)
	}

	return out
}

// scripts maps each process of src to its script.
func scripts(t *testing.T, file, src string) map[string]string {
	t.Helper()

	out := map[string]string{}

	for _, body := range blocks(src, "process ") {
		name := strings.Fields(body)[0]

		for _, label := range []string{"\n    script:\n", "\n    shell:\n", "\n    exec:\n"} {
			if _, script, ok := strings.Cut(body, label); ok {
				out[name] = script
			}
		}

		if out[name] == "" {
			t.Errorf("%s: process %s has no script", file, name)
		}
	}

	if len(out) == 0 {
		t.Errorf("%s declares no process", file)
	}

	return out
}

// TestAWSBatchProfilePollsEveryTwoSeconds: the awsbatch profile checks job
// status every 2 s rather than Nextflow's 10 s Batch default (#537), scoped
// to the awsbatch executor so a local executor keeps its 100 ms.
func TestAWSBatchProfilePollsEveryTwoSeconds(t *testing.T) {
	tree := readTree(t, emitFixture(t, "diamond_min"))
	cfg := tree["nextflow.config"]

	const poll = "        executor.$awsbatch.pollInterval = '2 sec'\n"

	awsbatch := cfg[strings.Index(cfg, "    awsbatch {"):strings.Index(cfg, "    healthomics {")]
	if !strings.Contains(awsbatch, poll) {
		t.Errorf("the awsbatch profile lacks %q:\n%s", poll, awsbatch)
	}

	if strings.Count(cfg, "pollInterval =") != 1 {
		t.Errorf("nextflow.config sets a poll interval outside the awsbatch profile:\n%s", cfg)
	}

	for _, name := range []string{"conf/container.config", "conf/healthomics.config"} {
		if strings.Contains(tree[name], "pollInterval =") {
			t.Errorf("%s sets a poll interval:\n%s", name, tree[name])
		}
	}
}

// TestEmitHealthOmicsTemplate: the HealthOmics parameter template declares
// every entry input and the run's mrp --overrides file, optional, and none
// of the project's other params, which a run then cannot set: the image
// the package pins (#334), and the file values it stages. Its bytes are
// those a registered workflow was created with. The package's own config
// is conf/base.config, the pinned image and the healthomics profile's
// config, without the project's profiles (#571).
func TestEmitHealthOmicsTemplate(t *testing.T) {
	tree := readTree(t, emitFixture(t, "entry_file"))

	const want = `{
  "mro2nf_overrides": {
    "description": "an s3:// URI of mrp's --overrides file, over each stage's resources",
    "optional": true
  },
  "reads": {
    "description": "pipeline input \"reads\"; the transpiled value when unset; a file value is an s3:// URI, or an s3:// prefix for a directory",
    "optional": true
  },
  "scale": {
    "description": "pipeline input \"scale\"; the transpiled value when unset; a file value is an s3:// URI, or an s3:// prefix for a directory",
    "optional": true
  }
}
`

	if d := cmp.Diff(want, tree["parameter-template.json"]); d != "" {
		t.Errorf("parameter template (-want +got):\n%s", d)
	}

	if strings.Contains(tree["package.sh"], "nextflow_schema.json") {
		t.Error("package.sh packages nextflow_schema.json, from which HealthOmics would detect params the template does not name")
	}

	pkg := tree["package.sh"]

	for _, want := range []string{
		"includeConfig 'conf/base.config'\n",
		`params.mro2nf_image = '$1'`,
		"includeConfig 'conf/healthomics.config'",
		"@sha256:[0-9a-f]{64}$",
		"cp -R main.nf stage.nf conf _assets parameter-template.json \"$pkg\"/\n",
		"cat > \"$pkg/nextflow.config\" <<EOF\n",
		"if [ -d subworkflows ]; then\n    cp -R subworkflows",
	} {
		if !strings.Contains(pkg, want) {
			t.Errorf("package.sh lacks %q:\n%s", want, pkg)
		}
	}

	if strings.Index(pkg, "includeConfig 'conf/base.config'") > strings.Index(pkg, "params.mro2nf_image = '$1'") ||
		strings.Index(pkg, "params.mro2nf_image = '$1'") > strings.Index(pkg, "includeConfig 'conf/healthomics.config'") {
		t.Error("package.sh pins the image before the base config or after the profile that reads it")
	}
}

// TestDockerProfileRunsAsInvokingUser: the docker profile runs each
// container as the invoking user with a writable HOME so work/ is not
// root-owned (#519), through process.containerOptions so a -c
// docker.runOptions adds to it; only the docker profile maps the user, as
// AWS Batch runs the container on its own instance.
func TestDockerProfileRunsAsInvokingUser(t *testing.T) {
	tree := readTree(t, emitFixture(t, "diamond_min"))
	cfg := tree["nextflow.config"]

	const opts = "process.containerOptions = '-u $(id -u):$(id -g) -e HOME=/tmp'\n"

	docker := cfg[strings.Index(cfg, "    docker {"):strings.Index(cfg, "    awsbatch {")]
	if !strings.Contains(docker, opts) {
		t.Errorf("the docker profile lacks %q:\n%s", opts, docker)
	}

	if strings.Count(cfg, "process.containerOptions =") != 1 || strings.Contains(cfg, "docker.runOptions =") {
		t.Errorf("nextflow.config sets container options outside the docker profile:\n%s", cfg)
	}

	for _, name := range []string{"conf/container.config", "conf/healthomics.config"} {
		if strings.Contains(tree[name], "Options") {
			t.Errorf("%s sets container options, which AWS Batch and HealthOmics do not take from a user mapping:\n%s", name, tree[name])
		}
	}
}

// TestLocalProfilesGrantFromThePool: the profiles that run every task on
// one host, standard and docker, grant each job from the pool their
// process.resourceLimits clamp a task to, the host's cpus and 90% of its
// memory as mrp's local job manager defaults to; every other profile
// grants it as mrp's cluster job managers do (#545). PLAN and every stage
// task hand mre the pool of their own task.
func TestLocalProfilesGrantFromThePool(t *testing.T) {
	tree := readTree(t, emitFixture(t, "diamond_min"))
	cfg, base := tree["nextflow.config"], tree["conf/base.config"]

	const (
		limits = "        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() * 0.9 ]\n" +
			"        params.mro2nf_local_jobs = true\n"
		off = "params.mro2nf_local_jobs = false\n"
	)

	_, profiles, _ := strings.Cut(cfg, "profiles {")
	for _, name := range []string{"standard", "docker"} {
		_, body, _ := strings.Cut(profiles, "    "+name+" {")
		body, _, _ = strings.Cut(body, "\n    }\n")
		body += "\n"

		if !strings.Contains(body, limits) {
			t.Errorf("the %s profile lacks %q:\n%s", name, limits, body)
		}
	}

	if strings.Count(cfg, "params.mro2nf_local_jobs = true") != 2 || !strings.Contains(base, off) {
		t.Errorf("nextflow.config grants from a local pool outside the standard and docker profiles:\n%s\n%s", base, cfg)
	}

	for _, local := range []string{"resourceLimits =", "executor ="} {
		if strings.Contains(base, local) {
			t.Errorf("conf/base.config, which the HealthOmics package includes, sets %q:\n%s", local, base)
		}
	}

	for _, name := range []string{"conf/container.config", "conf/healthomics.config"} {
		if strings.Contains(tree[name], "mro2nf_local_jobs") {
			t.Errorf("%s sets the local pool:\n%s", name, tree[name])
		}
	}

	if !strings.Contains(tree["main.nf"], "${pool(task, meta.run.local_jobs)} -call call ") {
		t.Errorf("PLAN does not hand mre its task's pool:\n%s", tree["main.nf"])
	}

	if !strings.Contains(stageModule, "-work .${pool(task, run.local_jobs)}\"") {
		t.Errorf("mreStage does not hand mre its task's pool:\n%s", stageModule)
	}
}
