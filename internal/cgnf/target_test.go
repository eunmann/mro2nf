package cgnf

import (
	"encoding/json"
	"strings"
	"testing"
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
	cfg, ctr, ho := tree["nextflow.config"], tree["conf/container.config"], tree["conf/healthomics.config"]

	for _, want := range []string{
		"params.mro2nf_mre = 'mre'\n",
		"params.mro2nf_image = null\nparams.mro2nf_require_image = false\n",
		"    docker {\n        process.executor = 'local'\n",
		"        docker.enabled = true\n        process.containerOptions = ",
		"'\n        includeConfig 'conf/container.config'\n",
		"    awsbatch {\n        process.executor = 'awsbatch'\n        includeConfig 'conf/container.config'\n        params.mro2nf_host_inputs = false\n",
		"    healthomics {\n        includeConfig 'conf/healthomics.config'\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("nextflow.config lacks %q:\n%s", want, cfg)
		}
	}

	if strings.Index(cfg, "profiles {") < strings.Index(cfg, "params.mro2nf_image") {
		t.Error("nextflow.config sets a param after its profiles, which would override theirs")
	}

	for _, want := range []string{
		"params.mro2nf_confine = true\n",
		"params.mro2nf_require_image = true\n",
		"process.container = params.mro2nf_image\n",
	} {
		if !strings.Contains(ctr, want) {
			t.Errorf("conf/container.config lacks %q:\n%s", want, ctr)
		}
	}

	if strings.Contains(ctr, "withName") {
		t.Errorf("conf/container.config gives a task an image of its own:\n%s", ctr)
	}

	for _, tool := range []string{"params.mro2nf_mre", "params.mro2nf_mrjob", "params.mro2nf_adapters"} {
		if strings.Contains(ctr, tool) {
			t.Errorf("conf/container.config names %s, where the image's PATH holds mre:\n%s", tool, ctr)
		}
	}

	for _, want := range []string{"includeConfig 'container.config'\n", "params.mro2nf_host_inputs = false\n", "params.outdir = '/mnt/workflow/pubdir'\n"} {
		if !strings.Contains(ho, want) {
			t.Errorf("conf/healthomics.config lacks %q:\n%s", want, ho)
		}
	}

	main := tree["main.nf"]
	if !strings.Contains(main, "if (on(params.mro2nf_require_image) && !params.mro2nf_image) {") || strings.Index(main, "!params.mro2nf_image") > strings.Index(main, "PLAN(") {
		t.Errorf("main.nf does not refuse a container profile without an image before PLAN:\n%s", main)
	}

	for _, want := range []string{"stage -phase ${phase} ${graph(meta, '')}", "outs ${graph(meta, 'mro')}"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
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
	cfg := tree["nextflow.config"]

	if !strings.Contains(cfg, "params.mro2nf_s3_mount = null\n") || strings.Index(cfg, "params.mro2nf_s3_mount = null") > strings.Index(cfg, "profiles {") {
		t.Errorf("nextflow.config does not leave the S3 mount unset before its profiles:\n%s", cfg)
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

	if strings.Count(cfg, "aws.batch.volumes") != 1 || strings.Count(cfg, "mro2nf_s3_mount = ") != 2 {
		t.Errorf("a profile other than awsbatch mounts S3:\n%s", cfg)
	}

	for _, f := range []string{"conf/container.config", "conf/healthomics.config"} {
		if strings.Contains(tree[f], "mro2nf_s3_mount") || strings.Contains(tree[f], "aws.batch.volumes") {
			t.Errorf("%s mounts S3:\n%s", f, tree[f])
		}
	}

	if want := "if (params.mro2nf_s3_mount) {\n        cmd += \" -s3-mount ${shq(params.mro2nf_s3_mount)}\""; !strings.Contains(stageModule, want) {
		t.Errorf("stage.nf lacks %q", want)
	}

	for _, want := range []string{"process PLAN {\n    cache !params.mro2nf_s3_mount\n", "${params.mro2nf_s3_mount ? ' -s3-mount ' + shq(params.mro2nf_s3_mount) : ''}"} {
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
	cfg, ctr := tree["nextflow.config"], tree["conf/container.config"]

	top, _, _ := strings.Cut(cfg, "profiles {")
	if strings.Count(cfg, "mro2nf_host_in_place") != 1 || !strings.Contains(top, "params.mro2nf_host_in_place = true\n") {
		t.Errorf("nextflow.config does not read head-node inputs in place before its profiles alone:\n%s", cfg)
	}

	if !strings.Contains(ctr, "params.mro2nf_host_in_place = false\n") || strings.Contains(tree["conf/healthomics.config"], "mro2nf_host_in_place") {
		t.Errorf("conf/container.config does not stage head-node inputs, alone:\n%s", ctr)
	}

	for _, want := range []string{" -host-in-place=${on(params.mro2nf_host_in_place)} ", " -run-workdir ${shq(workflow.workDir.toUriString())}"} {
		if !strings.Contains(tree["main.nf"], want) {
			t.Errorf("PLAN lacks %q:\n%s", want, tree["main.nf"])
		}
	}

	if want := "if (on(params.mro2nf_host_in_place)) {\n        cmd += ' -host-in-place'"; !strings.Contains(stageModule, want) {
		t.Errorf("stage.nf lacks %q", want)
	}
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
// the package pins (#334), and the file values it stages.
func TestEmitHealthOmicsTemplate(t *testing.T) {
	tree := readTree(t, emitFixture(t, "entry_file"))

	var tmpl map[string]templateParam
	if err := json.Unmarshal([]byte(tree["parameter-template.json"]), &tmpl); err != nil {
		t.Fatal(err)
	}

	if len(tmpl) != 3 || !tmpl["reads"].Optional || !tmpl["scale"].Optional || !tmpl["mro2nf_overrides"].Optional {
		t.Errorf("parameter template %v, want reads, scale and mro2nf_overrides, optional", tmpl)
	}

	for _, want := range []string{
		`params.mro2nf_image = '$1'`,
		"includeConfig 'conf/healthomics.config'",
		"@sha256:[0-9a-f]{64}$",
		"cp -R main.nf stage.nf nextflow.config conf _assets parameter-template.json",
		"if [ -d subworkflows ]; then\n    cp -R subworkflows",
	} {
		if !strings.Contains(tree["package.sh"], want) {
			t.Errorf("package.sh lacks %q:\n%s", want, tree["package.sh"])
		}
	}

	if strings.Index(tree["package.sh"], "params.mro2nf_image = '$1'") > strings.Index(tree["package.sh"], "includeConfig 'conf/healthomics.config'") {
		t.Error("package.sh pins the image after the profile reads it")
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
	cfg := tree["nextflow.config"]

	const (
		limits = "        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() * 0.9 ]\n" +
			"        params.mro2nf_local_jobs = true\n"
		off = "params.mro2nf_local_jobs = false\n"
	)

	top, profiles, _ := strings.Cut(cfg, "profiles {")
	for _, name := range []string{"standard", "docker"} {
		_, body, _ := strings.Cut(profiles, "    "+name+" {")
		body, _, _ = strings.Cut(body, "\n    }\n")
		body += "\n"

		if !strings.Contains(body, limits) {
			t.Errorf("the %s profile lacks %q:\n%s", name, limits, body)
		}
	}

	if strings.Count(cfg, "params.mro2nf_local_jobs = true") != 2 || !strings.Contains(top, off) {
		t.Errorf("nextflow.config grants from a local pool outside the standard and docker profiles:\n%s", cfg)
	}

	for _, name := range []string{"conf/container.config", "conf/healthomics.config"} {
		if strings.Contains(tree[name], "mro2nf_local_jobs") {
			t.Errorf("%s sets the local pool:\n%s", name, tree[name])
		}
	}

	if !strings.Contains(tree["main.nf"], "${pool(task)} -call call ") {
		t.Errorf("PLAN does not hand mre its task's pool:\n%s", tree["main.nf"])
	}

	if !strings.Contains(stageModule, "-work .${pool(task)}\"") {
		t.Errorf("mreStage does not hand mre its task's pool:\n%s", stageModule)
	}
}
