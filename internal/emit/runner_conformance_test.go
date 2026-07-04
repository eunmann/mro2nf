package emit

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// TestRunnerPythonUnit runs the runner's own pure-python unit tests (flag
// parser, Go-JSON encoder, walks, martian API contract) through the standard
// `make test` gate.
func TestRunnerPythonUnit(t *testing.T) {
	requirePython(t)

	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "runner", "-p", "test_*.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python unit tests failed: %v\n%s", err, out)
	}
}

// TestRunnerConformance is the #79 byte-parity guard: identical staged inputs
// go through `mre <phase>` AND run_stage.py, and every produced artifact —
// outs bundles (data.json + f/ leaves), chunks.json, joinres.json, chunk
// bundles — must be byte-identical. Each phase's runner leg reads the
// mre-produced upstream artifacts, so a drift on either side is caught and
// localized (the e2e goldens alone could be fooled by a compensating-error
// pair where the runner consistently reads its own miswritten bundles).
func TestRunnerConformance(t *testing.T) {
	requirePython(t)

	env := newConformanceEnv(t)

	mreSplit := env.runPhase(t, "mre", "split", nil)
	pySplit := env.runPhase(t, "py", "split", nil)
	diffDirs(t, "split", mreSplit, pySplit)

	chunk := filepath.Join(mreSplit, "chunk_00000")
	mreMain := env.runPhase(t, "mre", "main", []string{"-chunk", chunk})
	pyMain := env.runPhase(t, "py", "main", []string{"-chunk", chunk})
	diffDirs(t, "main", mreMain, pyMain)

	joinFlags := []string{
		"-chunkdefs", filepath.Join(mreSplit, "chunks.json"),
		"-chunkouts", filepath.Join(mreMain, "outs"),
	}
	mreJoin := env.runPhase(t, "mre", "join", joinFlags)
	pyJoin := env.runPhase(t, "py", "join", joinFlags)
	diffDirs(t, "join", mreJoin, pyJoin)
}

// TestRunnerConformanceExitCodes pins the exit-code matrix against mre: an
// ASSERT is 42 in both stacks, a stage sys.exit(0) in main succeeds with the
// skeleton outs bundle in both, and any other stage failure is 1 in both.
func TestRunnerConformanceExitCodes(t *testing.T) {
	requirePython(t)

	cases := []struct {
		name, body string
		wantExit   int
		wantBundle bool
	}{
		{"assert", "martian.exit('boom')", 42, false},
		{"sysexit zero", "import sys; sys.exit(0)", 0, true},
		{"sysexit nonzero", "import sys; sys.exit(5)", 1, false},
		{"raise", "raise RuntimeError('bad')", 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConformanceEnvWithMain(t, "import martian\n\ndef main(args, outs):\n    "+tc.body+"\n")

			mreCode, mreOut := env.runPhaseCode(t, "mre", "main", nil)
			pyCode, pyOut := env.runPhaseCode(t, "py", "main", nil)

			if mreCode != tc.wantExit || pyCode != tc.wantExit {
				t.Errorf("exit codes: mre=%d py=%d, want both %d", mreCode, pyCode, tc.wantExit)
			}

			mreBundle := fileExistsAt(filepath.Join(mreOut, "outs", "data.json"))
			pyBundle := fileExistsAt(filepath.Join(pyOut, "outs", "data.json"))

			if mreBundle != tc.wantBundle || pyBundle != tc.wantBundle {
				t.Errorf("outs bundle present: mre=%v py=%v, want both %v", mreBundle, pyBundle, tc.wantBundle)
			}

			if tc.wantBundle && mreBundle && pyBundle {
				diffDirs(t, "sysexit-zero outs", filepath.Join(mreOut, "outs"), filepath.Join(pyOut, "outs"))
			}
		})
	}
}

// conformanceEnv is one prepared stage + inputs shared by both stacks.
type conformanceEnv struct {
	mre, runner, shell, typesFile, argsDir, stageCode, root string
	callable                                                string
}

// requirePython skips when python3 is unavailable (the runner is python-only).
func requirePython(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// conformanceStage is a split stage whose values exercise the parity trouble
// spots: float fidelity (42.0), non-ASCII map KEYS in chunk defs (Go vs python
// escaping), a file output, and chunk resource overrides.
const conformanceStage = `import martian

def split(args):
    return {
        'chunks': [
            {'i': 0, 'café': 'kéy', '__mem_gb': 2},
            {'i': 1, 'café': 'v2'},
        ],
        'join': {'__threads': 2},
    }

def main(args, outs):
    outs.part = args.value * 10.0 + 42.0
    p = martian.make_path('report.txt').decode()
    with open(p, 'w') as f:
        f.write('value=%s threads=%s' % (args.value, martian.get_threads_allocation()))
    outs.report = p

def join(args, outs, chunk_defs, chunk_outs):
    outs.total = sum(c.part for c in chunk_outs)
    outs.report = chunk_outs[0].report
`

func newConformanceEnv(t *testing.T) *conformanceEnv {
	t.Helper()

	return buildConformanceEnv(t, conformanceStage, &ir.Stage{
		Name:  "CONF",
		In:    []ir.Param{{Name: "value", Type: "float", BaseType: "float"}},
		Out:   []ir.Param{{Name: "total", Type: "float", BaseType: "float"}, {Name: "report", Type: "txt", BaseType: "txt", IsFile: true}},
		Split: true,
		ChunkIn: []ir.Param{
			{Name: "i", Type: "int", BaseType: "int"},
			{Name: "café", Type: "string", BaseType: "string"},
		},
		ChunkOut: []ir.Param{{Name: "part", Type: "float", BaseType: "float"}, {Name: "report", Type: "txt", BaseType: "txt", IsFile: true}},
		Lang:     ir.LangPy,
	})
}

func newConformanceEnvWithMain(t *testing.T, code string) *conformanceEnv {
	t.Helper()

	return buildConformanceEnv(t, code, &ir.Stage{
		Name: "CONF",
		In:   []ir.Param{{Name: "value", Type: "float", BaseType: "float"}},
		Out:  []ir.Param{{Name: "total", Type: "float", BaseType: "float"}},
		Lang: ir.LangPy,
	})
}

// buildConformanceEnv builds mre, stages the shared inputs (types.json + args
// bundle + stage module), and locates the adapter + runner.
func buildConformanceEnv(t *testing.T, code string, s *ir.Stage) *conformanceEnv {
	t.Helper()

	root := t.TempDir()
	env := &conformanceEnv{root: root, callable: s.Name}

	env.mre = filepath.Join(root, "mre")
	build := exec.Command("go", "build", "-o", env.mre, "github.com/eunmann/mro2nf/cmd/mre")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mre: %v\n%s", err, out)
	}

	for name, dst := range map[string]*string{
		"../../vendor-martian/python/martian_shell.py": &env.shell,
		"runner/run_stage.py":                          &env.runner,
	} {
		abs, err := filepath.Abs(name)
		if err != nil {
			t.Fatal(err)
		}

		*dst = abs
	}

	prog := &ir.Program{Stages: map[string]*ir.Stage{s.Name: s}, Pipelines: map[string]*ir.Pipeline{}}

	env.typesFile = filepath.Join(root, "types.json")
	if err := types.BuildManifest(prog).Write(env.typesFile); err != nil {
		t.Fatal(err)
	}

	env.argsDir = filepath.Join(root, "args")
	man := types.BuildManifest(prog)
	if err := shim.WriteBundle(env.argsDir, map[string]any{"value": 1.5}, man.Params(s.Name, types.RoleIn), man.Table()); err != nil {
		t.Fatal(err)
	}

	env.stageCode = filepath.Join(root, "stages", "conf")
	if err := os.MkdirAll(env.stageCode, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.stageCode, "__init__.py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}

	return env
}

// runPhase runs one stack's phase and fails the test on a non-zero exit.
func (e *conformanceEnv) runPhase(t *testing.T, stack, phase string, extra []string) string {
	t.Helper()

	code, dir := e.runPhaseCode(t, stack, phase, extra)
	if code != 0 {
		t.Fatalf("%s %s exited %d", stack, phase, code)
	}

	return dir
}

// runPhaseCode runs one stack's phase in a fresh work dir, returning the exit
// code and the dir. Both stacks get the identical flag tail the emitter
// appends (see stageCmd), differing only in the head.
func (e *conformanceEnv) runPhaseCode(t *testing.T, stack, phase string, extra []string) (int, string) {
	t.Helper()

	dir := filepath.Join(e.root, stack+"_"+phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	role, outsList := types.RoleMainOut, "part,report"
	outFlag := "outs"

	switch phase {
	case "split":
		role, outFlag = types.RoleChunkIn, ""
		extra = append(extra, "-o", "chunks.json", "-joinres", "joinres.json", "-chunkdir", ".")
	case "join":
		role, outsList = types.RoleOut, "total,report"
	}

	args := []string{phase, "-stagecode", e.stageCode, "-call", "CONF", "-mro", "pipeline.mro"}
	if stack == "mre" {
		args = append([]string{phase, "-shell", e.shell, "-stagecode", e.stageCode, "-lang", "py", "-call", "CONF", "-mro", "pipeline.mro"}, args[5:]...)
	}

	args = append(args, "-args", e.argsDir, "-types", e.typesFile, "-callable", e.callable, "-role", role,
		"-threads", "1", "-memgb", "1", "-work", ".")
	if outFlag != "" {
		args = append(args, "-outs", outsList, "-o", outFlag)
	}

	args = append(args, extra...)

	bin := e.mre
	if stack == "py" {
		bin = "python3"
		args = append([]string{e.runner}, args...)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, dir
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), dir
	}

	t.Fatalf("%s %s: %v\n%s", stack, phase, err, out)

	return -1, dir
}

// diffDirs byte-compares every regular file under two directories (relative
// tree + content), ignoring only the phase scratch dirs the stacks lay out
// differently is NOT allowed — the full tree must match.
func diffDirs(t *testing.T, what, a, b string) {
	t.Helper()

	fa, fb := treeFiles(t, a), treeFiles(t, b)

	for rel := range fa {
		if _, ok := fb[rel]; !ok {
			t.Errorf("%s: %s only in mre output", what, rel)
		}
	}

	for rel := range fb {
		if _, ok := fa[rel]; !ok {
			t.Errorf("%s: %s only in runner output", what, rel)
		}
	}

	for rel := range fa {
		if _, ok := fb[rel]; !ok {
			continue
		}

		da, err := os.ReadFile(filepath.Join(a, rel))
		if err != nil {
			t.Fatal(err)
		}

		db, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(da, db) {
			t.Errorf("%s: %s differs\nmre: %q\npy:  %q", what, rel, truncate(da), truncate(db))
		}
	}
}

// treeFiles maps the relative paths of regular files under root, skipping the
// per-phase scratch dirs (split/, main/, join/) whose tmp contents are not
// part of the produced-artifact contract.
func treeFiles(t *testing.T, root string) map[string]bool {
	t.Helper()

	out := map[string]bool{}

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		top, _, _ := strings.Cut(rel, string(filepath.Separator))
		if top == "split" || top == "main" || top == "join" {
			return nil
		}

		out[rel] = true

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func truncate(b []byte) string {
	const limit = 400
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}

	return string(b)
}

func fileExistsAt(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
