package emit_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
	"github.com/eunmann/mro2nf/internal/frontend"
)

// buildMre builds the real mre binary into a temp dir (the -native path bakes
// entry args by exec'ing it at emit time). Skips the test if the build fails.
func buildMre(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "mre")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/mre")
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mre unavailable: %v\n%s", err, b)
	}

	return out
}

// processHeadRE matches an emitted `process NAME {` header at column 0.
var processHeadRE = regexp.MustCompile(`(?m)^process\s+(\S+)\s*\{`)

// projectProcesses returns every emitted process, keyed by name, mapped to its
// full block text (header through the balanced closing brace), across main.nf and
// every module .nf under dir.
func projectProcesses(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	var files []string
	if m, _ := filepath.Glob(filepath.Join(dir, "main.nf")); m != nil {
		files = append(files, m...)
	}

	mods, _ := filepath.Glob(filepath.Join(dir, "modules", "*.nf"))
	files = append(files, mods...)

	for _, f := range files {
		content := readFile(t, f)
		for _, loc := range processHeadRE.FindAllStringSubmatchIndex(content, -1) {
			name := content[loc[2]:loc[3]]
			out[name] = processBlock(content[loc[0]:])
		}
	}

	return out
}

// processBlock returns the process block starting at s[0] up to and including the
// brace that balances the header's opening `{`.
func processBlock(s string) string {
	depth := 0
	for i, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}

	return s
}

// TestProcessRoleLabelPartition guards #226's core invariant: every emitted
// process is labelled with exactly one container role — a stage process by its
// `label 'lang_*'` (the stage image), a pure-mre data-plane process by
// `label 'role_dataplane'` (the slim image). A process with neither would silently
// inherit the default (stage) image with no way to retarget it; one with both
// would be ambiguous. Run across shapes that exercise every generator family, in
// the default, -native, and -native-runner emission modes.
func TestProcessRoleLabelPartition(t *testing.T) {
	fixtures := []string{"split_test", "fanin", "empty_map_fork", "cellranger_shaped"}
	modes := []struct {
		name string
		opts emit.Options
	}{
		{"default", emit.Options{}},
		{"native", emit.Options{Native: true, Mre: buildMre(t)}},
		{"native-runner", emit.Options{NativeRunner: true}},
	}

	for _, fx := range fixtures {
		for _, m := range modes {
			t.Run(fx+"/"+m.name, func(t *testing.T) {
				procs := projectProcesses(t, emitFixtureOpts(t, fx, m.opts))
				if len(procs) == 0 {
					t.Fatalf("%s emitted no processes", fx)
				}

				for name, block := range procs {
					dataplane := strings.Contains(block, "label 'role_dataplane'")
					stage := strings.Contains(block, "label 'lang_")

					switch {
					case dataplane && stage:
						t.Errorf("process %s carries BOTH a stage and a dataplane role label", name)
					case !dataplane && !stage:
						t.Errorf("process %s carries NO container-role label (would silently use the stage image)", name)
					}
				}
			})
		}
	}
}

// TestDataplaneRoleClassification pins the specific role of the well-known
// processes in the split fixture, so a future refactor that mis-files a task's
// image (e.g. a BIND that starts pulling the heavy stage image) fails loudly.
func TestDataplaneRoleClassification(t *testing.T) {
	procs := projectProcesses(t, loadAndEmit(t))

	for name, block := range procs {
		isDataplane := strings.Contains(block, "label 'role_dataplane'")

		switch {
		// bind/layout tasks run only mre — they are dataplane.
		case strings.HasPrefix(name, "BIND_") || name == "LAYOUT":
			if !isDataplane {
				t.Errorf("%s should carry the dataplane role label", name)
			}
		// The split triad runs stage code — it must NOT be dataplane.
		case strings.Contains(name, "SUM_SQUARES"):
			if isDataplane {
				t.Errorf("stage process %s must not carry the dataplane role label", name)
			}
		}
	}
}

// emitFixtureOpts parses/lowers a fixture and emits it with the given Options
// (OutDir and the tool paths are filled in), returning the output dir.
func emitFixtureOpts(t *testing.T, fixture string, opts emit.Options) string {
	t.Helper()

	base := "../../testdata/" + fixture
	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	dir := t.TempDir()
	opts.OutDir = dir
	// A caller that needs a real binary (the -native bake) sets opts.Mre; the other
	// modes never exec it, so a placeholder name is fine.
	if opts.Mre == "" {
		opts.Mre = "mre"
	}
	opts.Shell = "/x/martian_shell.py"
	opts.MROFile = "pipeline.mro"
	opts.MRODir = base

	code := map[string]string{}
	for n := range prog.Stages {
		code[n] = "/x/" + n
	}
	opts.StageCode = code

	if err := emit.Emit(prog, opts); err != nil {
		t.Fatalf("emit %s: %v", fixture, err)
	}

	return dir
}

// TestConfigPerRoleContainer checks the container config wires a per-role image:
// the default (no dataplane image) keeps a single-image project (null dataplane
// param, falling back to params.container), while a supplied dataplane image is
// exposed as its own param. The withLabel selector is present in both so a
// data-plane task always resolves to the right image.
func TestConfigPerRoleContainer(t *testing.T) {
	emitConfig := func(dataplane string) string {
		base := "../../testdata/split_test"
		ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
		if err != nil {
			t.Fatal(err)
		}
		prog, err := frontend.Lower(ast, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		mre, shell, stages := realRuntime(t)
		if err := emit.Emit(prog, emit.Options{
			OutDir: dir, Mre: mre, Shell: shell, MROFile: "pipeline.mro",
			Target: emit.TargetHealthOmics, Container: "ecr/stage:1",
			ContainerDataplane: dataplane, StageCode: stages,
		}); err != nil {
			t.Fatalf("emit: %v", err)
		}

		return readFile(t, filepath.Join(dir, "nextflow.config"))
	}

	// Single-image default: dataplane param null, still selectable, falls back.
	single := emitConfig("")
	for _, want := range []string{
		"params.container = 'ecr/stage:1'",
		"params.container_dataplane = null",
		"withLabel:role_dataplane { container = params.container_dataplane ?: params.container }",
	} {
		if !strings.Contains(single, want) {
			t.Errorf("single-image config missing %q\n%s", want, single)
		}
	}

	// Explicit dataplane image: its own param value.
	two := emitConfig("ecr/dataplane:1")
	if !strings.Contains(two, "params.container_dataplane = 'ecr/dataplane:1'") {
		t.Errorf("two-image config missing the dataplane image param\n%s", two)
	}
}

// TestConfigLocalNoRoleParams checks the per-role container wiring is scoped to
// container targets: a local project must not gain a container_dataplane param or
// a withLabel:role_dataplane selector (there is no per-task pull to amortize).
func TestConfigLocalNoRoleParams(t *testing.T) {
	cfg := readFile(t, filepath.Join(loadAndEmit(t), "nextflow.config"))

	for _, unwanted := range []string{"container_dataplane", "withLabel:role_dataplane"} {
		if strings.Contains(cfg, unwanted) {
			t.Errorf("local config should not contain %q", unwanted)
		}
	}
}

// TestDataplaneDockerfile checks the container target emits a slim data-plane
// image alongside the stage image: it carries the mre binary at the same path the
// scripts bake but none of the stage toolkit (no adapters/stages copies), and the
// awsbatch variant adds the aws CLI for S3 staging while HealthOmics does not.
func TestDataplaneDockerfile(t *testing.T) {
	emitDockerfiles := func(target emit.Target) string {
		base := "../../testdata/split_test"
		ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
		if err != nil {
			t.Fatal(err)
		}
		prog, err := frontend.Lower(ast, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		mre, shell, stages := realRuntime(t)
		if err := emit.Emit(prog, emit.Options{
			OutDir: dir, Mre: mre, Shell: shell, MROFile: "pipeline.mro",
			Target: target, Container: "ecr/stage:1", StageCode: stages,
		}); err != nil {
			t.Fatalf("emit: %v", err)
		}

		return readFile(t, filepath.Join(dir, "Dockerfile.dataplane"))
	}

	omics := emitDockerfiles(emit.TargetHealthOmics)
	for _, want := range []string{"debian:bookworm-slim", "COPY runtime/mre /opt/mro2nf/mre"} {
		if !strings.Contains(omics, want) {
			t.Errorf("dataplane Dockerfile missing %q", want)
		}
	}
	// The slim image must not carry the stage toolkit.
	for _, unwanted := range []string{"/adapters", "/stages", "COPY runtime/adapters"} {
		if strings.Contains(omics, unwanted) {
			t.Errorf("dataplane Dockerfile should not copy stage toolkit (%q)", unwanted)
		}
	}
	if strings.Contains(omics, "awscli") {
		t.Error("healthomics dataplane image needs no aws CLI (managed filesystem)")
	}

	batch := emitDockerfiles(emit.TargetAWSBatch)
	if !strings.Contains(batch, "awscli") {
		t.Error("awsbatch dataplane image must include the aws CLI for S3 staging")
	}
}

// TestHealthOmicsDataplaneParam checks the data-plane image is a declared, optional
// HealthOmics run parameter, so its ECR access is validated before the run while
// omitting it still works (falls back to the stage image).
func TestHealthOmicsDataplaneParam(t *testing.T) {
	base := "../../testdata/split_test"
	ast, err := frontend.Parse(base+"/pipeline.mro", []string{base}, false)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mre, shell, stages := realRuntime(t)
	if err := emit.Emit(prog, emit.Options{
		OutDir: dir, Mre: mre, Shell: shell, MROFile: "pipeline.mro",
		Target: emit.TargetHealthOmics, Container: "ecr/stage:1", StageCode: stages,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	tmpl := readFile(t, filepath.Join(dir, "parameter-template.json"))
	if !strings.Contains(tmpl, "container_dataplane") {
		t.Errorf("parameter template missing container_dataplane\n%s", tmpl)
	}

	pkg := readFile(t, filepath.Join(dir, "package.sh"))
	if !strings.Contains(pkg, "Dockerfile.dataplane") {
		t.Error("package.sh must exclude Dockerfile.dataplane from the workflow zip")
	}
}
