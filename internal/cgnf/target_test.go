package cgnf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestEmitTargetsAsProfiles: one project runs on every target, chosen at
// launch with -profile (#309): the local and grid profiles find the tools
// and code on the host, the container profiles in the image, every stage
// task in the stage image and PLAN and OUTS, which run mre alone, in the
// runtime image (#226), the release's of the build that transpiled the
// project; a container profile without a stage image fails at once.
func TestEmitTargetsAsProfiles(t *testing.T) {
	tree := readTree(t, emitFixture(t, "diamond_min"))
	cfg, ctr, ho := tree["nextflow.config"], tree["conf/container.config"], tree["conf/healthomics.config"]

	for _, want := range []string{
		"params.mro2nf_mre = '/opt/mre'\n",
		"params.mro2nf_code = ''\n",
		"params.mro2nf_image = null\n",
		"params.mro2nf_runtime_image = 'ghcr.io/eunmann/mro2nf-runtime:test'\n",
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
		"params.mro2nf_mre = '/opt/mro2nf/mre'\n",
		"params.mro2nf_mrjob = '/opt/mro2nf/mrjob'\n",
		"params.mro2nf_adapters = '/opt/mro2nf/adapters'\n",
		"params.mro2nf_code = '/opt/mro2nf/code'\n",
		"params.mro2nf_confine = true\n",
		"    container = params.mro2nf_image\n",
		"    withName: 'PLAN|OUTS' {\n        container = params.mro2nf_runtime_image\n",
	} {
		if !strings.Contains(ctr, want) {
			t.Errorf("conf/container.config lacks %q:\n%s", want, ctr)
		}
	}

	for _, want := range []string{"includeConfig 'container.config'\n", "params.mro2nf_host_inputs = false\n", "params.outdir = '/mnt/workflow/pubdir'\n"} {
		if !strings.Contains(ho, want) {
			t.Errorf("conf/healthomics.config lacks %q:\n%s", want, ho)
		}
	}

	main := tree["main.nf"]
	if !strings.Contains(main, "if (params.mro2nf_code && !params.mro2nf_image) {") || strings.Index(main, "!params.mro2nf_image") > strings.Index(main, "PLAN(") {
		t.Errorf("main.nf does not refuse a container profile without a stage image before PLAN:\n%s", main)
	}

	for _, want := range []string{"graph(meta, params.mro2nf_code)", "outs ${graph(meta, 'mro')}"} {
		if !strings.Contains(stageModule, want) {
			t.Errorf("stage.nf lacks %q", want)
		}
	}
}

// TestEmitStageImageRecipe: the generated Dockerfile builds FROM the
// release runtime image, the one definition of /opt/mro2nf (#310), and adds
// each root of the pipeline's code at its host path below /opt/mro2nf/code
// from a named build context build.sh passes, so the project copies no
// code (include_test: the entry's dir holds its include and stage code).
func TestEmitStageImageRecipe(t *testing.T) {
	tree := readTree(t, emitFixture(t, "include_test"))
	dir := fixturePath(t, "include_test")

	for _, want := range []string{
		"ARG RUNTIME=ghcr.io/eunmann/mro2nf-runtime:test\nARG BASE=${RUNTIME}\nFROM ${RUNTIME} AS runtime\nFROM ${BASE}\nCOPY --from=runtime /opt/mro2nf /opt/mro2nf\n",
		"COPY --from=code0 . /opt/mro2nf/code" + dir + "\n",
	} {
		if !strings.Contains(tree["Dockerfile"], want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, tree["Dockerfile"])
		}
	}

	if strings.Contains(tree["Dockerfile"], "code1") {
		t.Errorf("Dockerfile copies a root below another:\n%s", tree["Dockerfile"])
	}

	if want := "--build-context code0='" + dir + "'"; !strings.Contains(tree["build.sh"], want) {
		t.Errorf("build.sh lacks %q:\n%s", want, tree["build.sh"])
	}

	if tree[".dockerignore"] != "*\n" {
		t.Errorf(".dockerignore %q sends the project to the daemon", tree[".dockerignore"])
	}

	info, err := os.Stat(filepath.Join(emitFixture(t, "include_test"), "build.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("build.sh is not executable: %v %v", info, err)
	}
}

// TestOutermost: a root below another is dropped, each root kept once.
func TestOutermost(t *testing.T) {
	got := outermost([]string{"/a/b", "/a", "/ab", "/a/b/c/", "/x", "/ab"})
	if d := cmp.Diff([]string{"/a", "/ab", "/x"}, got); d != "" {
		t.Errorf("outermost (-want +got):\n%s", d)
	}
}

// TestEmitHealthOmicsTemplate: the HealthOmics parameter template declares
// every entry input, optional, and none of the project's own params, which
// a run then cannot set: the images the package pins (#334), and the file
// values it stages.
func TestEmitHealthOmicsTemplate(t *testing.T) {
	tree := readTree(t, emitFixture(t, "entry_file"))

	var tmpl map[string]templateParam
	if err := json.Unmarshal([]byte(tree["parameter-template.json"]), &tmpl); err != nil {
		t.Fatal(err)
	}

	if len(tmpl) != 2 || !tmpl["reads"].Optional || !tmpl["scale"].Optional {
		t.Errorf("parameter template %v, want reads and scale, optional", tmpl)
	}

	for _, want := range []string{
		`params.mro2nf_image = '$1'`,
		`params.mro2nf_runtime_image = '$2'`,
		"includeConfig 'conf/healthomics.config'",
		"@sha256:[0-9a-f]{64}$",
		"cp -R main.nf stage.nf nextflow.config conf _assets parameter-template.json",
	} {
		if !strings.Contains(tree["package.sh"], want) {
			t.Errorf("package.sh lacks %q:\n%s", want, tree["package.sh"])
		}
	}

	if strings.Index(tree["package.sh"], "params.mro2nf_image = '$1'") > strings.Index(tree["package.sh"], "includeConfig 'conf/healthomics.config'") {
		t.Error("package.sh pins the images after the profile reads them")
	}
}
