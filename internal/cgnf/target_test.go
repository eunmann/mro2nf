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
		"        docker.enabled = true\n        includeConfig 'conf/container.config'\n",
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

	for _, want := range []string{"args ${graph(meta, '')}", "run -phase ${phase} ${graph(meta, '')}", "post ${graph(meta, '')}", "outs ${graph(meta, 'mro')}"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
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
