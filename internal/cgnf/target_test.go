package cgnf

import (
	"encoding/json"
	"os"
	"os/exec"
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
		"params.mro2nf_runtime_image = 'ghcr.io/eunmann/mro2nf-runtime:v1.2.3'\n",
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
		"ARG RUNTIME=ghcr.io/eunmann/mro2nf-runtime:v1.2.3\nARG BASE=${RUNTIME}\nFROM ${RUNTIME} AS runtime\nFROM ${BASE}\nCOPY --from=runtime /opt/mro2nf /opt/mro2nf\n",
		"COPY --from=code0 [\".\", \"/opt/mro2nf/code" + dir + "\"]\n",
	} {
		if !strings.Contains(tree["Dockerfile"], want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, tree["Dockerfile"])
		}
	}

	if strings.Contains(tree["Dockerfile"], "code1") {
		t.Errorf("Dockerfile copies a root below another:\n%s", tree["Dockerfile"])
	}

	if want := "--build-context code0='" + dir + "'"; !strings.Contains(tree["build.sh"], want) || strings.Contains(tree["build.sh"], "RUNTIME=\"*") {
		t.Errorf("build.sh lacks %q, or needs a runtime image named:\n%s", want, tree["build.sh"])
	}

	if tree[".dockerignore"] != "*\n" {
		t.Errorf(".dockerignore %q sends the project to the daemon", tree[".dockerignore"])
	}

	info, err := os.Stat(filepath.Join(emitFixture(t, "include_test"), "build.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("build.sh is not executable: %v %v", info, err)
	}
}

// TestReleaseRuntime: only a version a release publishes the runtime image
// under names it (release.yml: the v* tag); a git describe past a tag, a
// Go pseudo-version, a dirty tree and dev name none.
func TestReleaseRuntime(t *testing.T) {
	for v, want := range map[string]string{
		"v1.2.3":                             runtimeImage + ":v1.2.3",
		"v1.2.0-rc1":                         runtimeImage + ":v1.2.0-rc1",
		"v0.4.0-6-gb025b83":                  "",
		"v0.4.0-6-gb025b83-dirty":            "",
		"v0.4.0-dirty":                       "",
		"b025b83":                            "",
		"dev":                                "",
		"v0.0.0-20260101000000-abcdef123456": "",
		"v0.5.1-0.20260101000000-abcdef123456+dirty": "",
	} {
		if got := (&project{opts: Options{Version: v}}).releaseRuntime(); got != want {
			t.Errorf("runtime image of build %s = %q, want %q", v, got, want)
		}
	}
}

// TestEmitUnreleasedRuntime: a transpiler build no release published the
// runtime image of names none: the container profiles refuse to launch
// and build.sh to build until one is named, each saying how to build it.
func TestEmitUnreleasedRuntime(t *testing.T) {
	dir := fixturePath(t, "diamond_min")
	out := t.TempDir()

	if err := Emit(filepath.Join(dir, "pipeline.mro"), Options{OutDir: out, MroPaths: []string{dir}, Version: "v0.4.0-6-gb025b83-dirty"}); err != nil {
		t.Fatal(err)
	}

	tree := readTree(t, out)
	for _, c := range []struct{ file, want string }{
		{"nextflow.config", "params.mro2nf_runtime_image = null\n"},
		{"main.nf", "if (params.mro2nf_code && !params.mro2nf_runtime_image) {"},
		{"main.nf", "error 'a container profile runs PLAN and OUTS in params.mro2nf_runtime_image, which no release published for this transpiler build: build it with docker build -f deploy/runtime/Dockerfile"},
		{"Dockerfile", "ARG RUNTIME\nARG BASE=${RUNTIME}\n"},
		{"build.sh", "case \" $* \" in\n    *\" RUNTIME=\"* | *\"=RUNTIME=\"*) ;;\n"},
	} {
		if !strings.Contains(tree[c.file], c.want) {
			t.Errorf("%s lacks %q:\n%s", c.file, c.want, tree[c.file])
		}
	}

	if strings.Index(tree["main.nf"], "!params.mro2nf_runtime_image") > strings.Index(tree["main.nf"], "PLAN(") {
		t.Error("main.nf refuses a missing runtime image only after PLAN")
	}

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"tag"}, false},
		{[]string{"tag", "-q"}, false},
		{[]string{"tag", "--build-arg", "RUNTIME=rt"}, true},
		{[]string{"tag", "--build-arg=RUNTIME=rt"}, true},
	} {
		cmd := exec.Command("bash", append([]string{filepath.Join(out, "build.sh")}, c.args...)...)
		if msg, err := cmd.CombinedOutput(); (err == nil) != c.ok {
			t.Errorf("build.sh %v: %v %s, want ok %t", c.args, err, msg, c.ok)
		}
	}
}

// TestEmitStageImageAbsoluteSrc: the code of a stage whose src is absolute
// is copied to that path itself, where Martian's FindPath looks for it in
// the image unchanged, beside the entry's dir below /opt/mro2nf/code; each
// destination is a JSON string, so a path with a space stays one.
func TestEmitStageImageAbsoluteSrc(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Pipelines")
	code := filepath.Join(t.TempDir(), "shared", "stages", "abs")

	for _, d := range []string{dir, code} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(code, "__init__.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	mro := filepath.Join(dir, "pipeline.mro")
	if err := os.WriteFile(mro, []byte("stage ABS(\n    out int y,\n    src py \""+code+"\",\n)\n\ncall ABS()\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := Emit(mro, Options{OutDir: out, MroPaths: []string{dir}, Version: "test"}); err != nil {
		t.Fatal(err)
	}

	tree := readTree(t, out)
	for _, want := range []string{
		"COPY --from=code0 [\".\", \"/opt/mro2nf/code" + dir + "\"]\n",
		"COPY --from=code1 [\".\", \"" + code + "\"]\n",
	} {
		if !strings.Contains(tree["Dockerfile"], want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, tree["Dockerfile"])
		}
	}

	if want := "--build-context code1='" + code + "'"; !strings.Contains(tree["build.sh"], want) {
		t.Errorf("build.sh lacks %q:\n%s", want, tree["build.sh"])
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
