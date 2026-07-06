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

	// Guard the fixture's reach: the parity diff is vacuous for an escape or
	// staging branch the fixture no longer hits, so pin the trouble-spot bytes
	// on the mre side (the py side matches it byte-for-byte via diffDirs).
	mustContain(t, filepath.Join(mreSplit, "chunks.json"),
		`\u003c`, `\u003e`, `\u0026`, `\u0001`, `\u2028`, `\u2029`, "2.5")
	mustContain(t, filepath.Join(mreSplit, "joinres.json"), "1.25")

	chunk := filepath.Join(mreSplit, "chunk_00000")
	mustContain(t, filepath.Join(chunk, "data.json"), shim.FileMarker)

	mreMain := env.runPhase(t, "mre", "main", []string{"-chunk", chunk})
	pyMain := env.runPhase(t, "py", "main", []string{"-chunk", chunk})
	diffDirs(t, "main", mreMain, pyMain)
	mustContain(t, filepath.Join(mreMain, "outs", "data.json"),
		`\u003c`, `\u0026`, `\u0001`, `\u0002`, `\u2028`, `\u2029`)

	joinFlags := []string{
		"-chunkdefs", filepath.Join(mreSplit, "chunks.json"),
		"-chunkouts", filepath.Join(mreMain, "outs"),
	}
	mreJoin := env.runPhase(t, "mre", "join", joinFlags)
	pyJoin := env.runPhase(t, "py", "join", joinFlags)
	diffDirs(t, "join", mreJoin, pyJoin)
}

// TestRunnerConformanceRelativeOutPath is the #179 regression guard: a stage
// that sets an out-file leaf to a RELATIVE path (the file lands in files/, where
// the stage runs) must be handled identically by both stacks. mre's data plane
// runs in the task dir and stats the relative path there — absent — so it keeps
// the path string; the runner must match. Before the cwd-restore fix the runner's
// data plane inherited the stage's files/ cwd, found the file, and staged it —
// diverging from mre and mrp (silent, since leaf counts still matched).
func TestRunnerConformanceRelativeOutPath(t *testing.T) {
	requirePython(t)

	code := "import martian\n\n" +
		"def main(args, outs):\n" +
		"    with open('relative_out.txt', 'w') as fh:\n" +
		"        fh.write('x')\n" +
		"    outs.report = 'relative_out.txt'\n"

	env := buildConformanceEnv(t, code, &ir.Stage{
		Name: "CONF",
		In:   []ir.Param{{Name: "value", Type: "float", BaseType: "float"}},
		Out:  []ir.Param{{Name: "report", Type: "file", BaseType: "file", IsFile: true}},
		Lang: ir.LangPy,
	})

	mreMain := env.runPhase(t, "mre", "main", nil)
	pyMain := env.runPhase(t, "py", "main", nil)
	diffDirs(t, "relative-outpath main", mreMain, pyMain)

	// Reach guard: the parity diff is vacuous unless the relative-path-kept-as-raw
	// branch is actually hit. Pin that mre kept the raw string and did NOT stage a
	// file leaf (an absolute-path fixture would stage it and pass the diff for the
	// wrong reason). The py side matches byte-for-byte via diffDirs.
	mainData := filepath.Join(mreMain, "outs", "data.json")
	mustContain(t, mainData, "relative_out.txt")
	mustNotContain(t, mainData, shim.FileMarker)
}

// TestRunnerConformanceRelativeChunkPath is the split-phase leg of #179: a split
// that writes a chunk-in file at a RELATIVE path and returns it in a chunk def
// must be handled identically by both stacks. mre's data plane (run in the task
// dir) stats it as absent and keeps the raw string; the pre-fix runner, still in
// files/, would find and stage it — the same divergence as the main case, on the
// split side, which the main-only test above does not cover.
func TestRunnerConformanceRelativeChunkPath(t *testing.T) {
	requirePython(t)

	code := "import martian\n\n" +
		"def split(args):\n" +
		"    with open('rel_chunk.txt', 'w') as fh:\n" +
		"        fh.write('x')\n" +
		"    return {'chunks': [{'cfile': 'rel_chunk.txt'}]}\n\n" +
		"def main(args, outs):\n    outs.total = 1.0\n\n" +
		"def join(args, outs, chunk_defs, chunk_outs):\n    outs.total = 1.0\n"

	env := buildConformanceEnv(t, code, &ir.Stage{
		Name:     "CONF",
		In:       []ir.Param{{Name: "value", Type: "float", BaseType: "float"}},
		Out:      []ir.Param{{Name: "total", Type: "float", BaseType: "float"}},
		Split:    true,
		ChunkIn:  []ir.Param{{Name: "cfile", Type: "file", BaseType: "file", IsFile: true}},
		ChunkOut: []ir.Param{{Name: "part", Type: "float", BaseType: "float"}},
		Lang:     ir.LangPy,
	})

	mreSplit := env.runPhase(t, "mre", "split", nil)
	pySplit := env.runPhase(t, "py", "split", nil)
	diffDirs(t, "relative-chunkpath split", mreSplit, pySplit)

	// Reach guard: mre kept the relative chunk-in path as a raw string (absent in
	// the task dir) and staged no leaf; the runner must match, not stage it.
	chunkData := filepath.Join(mreSplit, "chunk_00000", "data.json")
	mustContain(t, chunkData, "rel_chunk.txt")
	mustNotContain(t, chunkData, shim.FileMarker)
}

// TestRunnerConformanceExitCodes pins the exit-code matrix against mre: an
// ASSERT is 42 in both stacks, a stage sys.exit(0) in main succeeds with the
// skeleton outs bundle in both, and any other stage failure is 1 in both.
// The split cases prove (not just claim — see run_stage.py's parse_stage_defs
// and run_split comments) the adapter-path behavior: a split that sys.exit(0)s
// dies before writing _stage_defs, and a falsy split return (None or {}) is
// serialized as "" by the vendor shell — mre fails loudly on both, so the
// runner must too, and neither stack may leave a chunks.json behind.
func TestRunnerConformanceExitCodes(t *testing.T) {
	requirePython(t)

	cases := []struct {
		name, phase, body string
		wantExit          int
		wantArtifact      bool
	}{
		{"assert", "main", "martian.exit('boom')", 42, false},
		{"sysexit zero", "main", "import sys; sys.exit(0)", 0, true},
		{"sysexit nonzero", "main", "import sys; sys.exit(5)", 1, false},
		{"raise", "main", "raise RuntimeError('bad')", 1, false},
		{"split sysexit zero", "split", "import sys; sys.exit(0)", 1, false},
		{"split returns none", "split", "return None", 1, false},
		{"split returns empty", "split", "return {}", 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env *conformanceEnv
			if tc.phase == "split" {
				env = newConformanceEnvWithSplit(t, tc.body)
			} else {
				env = newConformanceEnvWithMain(t, "import martian\n\ndef main(args, outs):\n    "+tc.body+"\n")
			}

			mreCode, mreOut := env.runPhaseCode(t, "mre", tc.phase, nil)
			pyCode, pyOut := env.runPhaseCode(t, "py", tc.phase, nil)

			if mreCode != tc.wantExit || pyCode != tc.wantExit {
				t.Errorf("exit codes: mre=%d py=%d, want both %d", mreCode, pyCode, tc.wantExit)
			}

			// The phase's primary artifact: main's outs bundle, split's chunks.json.
			artifact := []string{"outs", "data.json"}
			if tc.phase == "split" {
				artifact = []string{"chunks.json"}
			}

			mreArtifact := fileExistsAt(filepath.Join(mreOut, filepath.Join(artifact...)))
			pyArtifact := fileExistsAt(filepath.Join(pyOut, filepath.Join(artifact...)))

			if mreArtifact != tc.wantArtifact || pyArtifact != tc.wantArtifact {
				t.Errorf("%s present: mre=%v py=%v, want both %v",
					filepath.Join(artifact...), mreArtifact, pyArtifact, tc.wantArtifact)
			}

			if tc.wantArtifact && mreArtifact && pyArtifact {
				diffDirs(t, "sysexit-zero outs", filepath.Join(mreOut, "outs"), filepath.Join(pyOut, "outs"))
			}

			// Exit 42 alone is not the whole assert contract: the ASSERT line
			// is the user-visible message (and what failure classification
			// keys on), so both stacks must emit the identical one.
			if tc.name == "assert" {
				mreLine := assertLine(t, filepath.Join(mreOut, "combined.log"))
				pyLine := assertLine(t, filepath.Join(pyOut, "combined.log"))

				if mreLine == "" || mreLine != pyLine {
					t.Errorf("ASSERT line: mre=%q py=%q, want identical non-empty", mreLine, pyLine)
				}
			}
		})
	}
}

// assertLine returns the "ASSERT:..." payload from the first output line
// carrying one, or "" when none was emitted. The framing differs by design —
// the runner prints the bare adapter-style line, mre embeds it in its wrapped
// error ("mre: main phase: ...: ASSERT:boom") — but the payload from the
// prefix to end-of-line must match, since that is the user-visible assert
// message and what failure classification keys on.
func assertLine(t *testing.T, log string) string {
	t.Helper()

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(string(raw), "\n") {
		if i := strings.Index(line, "ASSERT"); i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}

	return ""
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
// escaping), a string chunk def + outs value hitting every hand-ported escape
// branch (HTML `<>&`, a control character, U+2028/U+2029 — _go_string in the
// chunk/outs bundles, _py_string in chunks.json's RawMessage passthrough), a
// file-typed chunk-in the split writes (write_chunk_bundles' mark_files vs
// shim.WriteChunkBundle staging), fractional resource overrides (GoFloat's
// formatting in chunks.json + joinres.json), and a file output.
const conformanceStage = `import martian

def split(args):
    src = args.srcdir + '/chunk_src.txt'
    with open(src, 'w') as f:
        f.write('chunk-input-payload')
    return {
        'chunks': [
            {'i': 0, 'café': 'kéy', 'html': 'a<b>&c\x01d\u2028e\u2029f',
             'src': src, '__mem_gb': 2.5},
            {'i': 1, 'café': 'v2', 'html': 'plain', 'src': src},
        ],
        'join': {'__threads': 2, '__mem_gb': 1.25},
    }

def main(args, outs):
    outs.part = args.value * 10.0 + 42.0
    p = martian.make_path('report.txt').decode()
    with open(args.src) as f:
        payload = f.read()
    with open(p, 'w') as f:
        f.write('value=%s threads=%s mem=%s src=%s' % (
            args.value, martian.get_threads_allocation(),
            martian.get_memory_allocation(), payload))
    outs.report = p
    outs.note = args.html + ' <main>&\x02'

def join(args, outs, chunk_defs, chunk_outs):
    outs.total = sum(c.part for c in chunk_outs)
    outs.report = chunk_outs[0].report
    outs.note = chunk_outs[0].note + ' <join>'
`

func newConformanceEnv(t *testing.T) *conformanceEnv {
	t.Helper()

	return buildConformanceEnv(t, conformanceStage, &ir.Stage{
		Name: "CONF",
		In: []ir.Param{
			{Name: "value", Type: "float", BaseType: "float"},
			{Name: "srcdir", Type: "string", BaseType: "string"},
		},
		Out: []ir.Param{
			{Name: "total", Type: "float", BaseType: "float"},
			{Name: "report", Type: "txt", BaseType: "txt", IsFile: true},
			{Name: "note", Type: "string", BaseType: "string"},
		},
		Split: true,
		ChunkIn: []ir.Param{
			{Name: "i", Type: "int", BaseType: "int"},
			{Name: "café", Type: "string", BaseType: "string"},
			{Name: "html", Type: "string", BaseType: "string"},
			{Name: "src", Type: "txt", BaseType: "txt", IsFile: true},
		},
		ChunkOut: []ir.Param{
			{Name: "part", Type: "float", BaseType: "float"},
			{Name: "report", Type: "txt", BaseType: "txt", IsFile: true},
			{Name: "note", Type: "string", BaseType: "string"},
		},
		Lang: ir.LangPy,
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

// newConformanceEnvWithSplit builds a minimal Split:true stage whose split
// body is the given statement, for split-phase exit-code parity cases.
func newConformanceEnvWithSplit(t *testing.T, splitBody string) *conformanceEnv {
	t.Helper()

	code := "import martian\n\ndef split(args):\n    " + splitBody +
		"\n\ndef main(args, outs):\n    outs.part = 1.0\n" +
		"\ndef join(args, outs, chunk_defs, chunk_outs):\n    outs.total = 1.0\n"

	return buildConformanceEnv(t, code, &ir.Stage{
		Name:     "CONF",
		In:       []ir.Param{{Name: "value", Type: "float", BaseType: "float"}},
		Out:      []ir.Param{{Name: "total", Type: "float", BaseType: "float"}},
		Split:    true,
		ChunkIn:  []ir.Param{{Name: "i", Type: "int", BaseType: "int"}},
		ChunkOut: []ir.Param{{Name: "part", Type: "float", BaseType: "float"}},
		Lang:     ir.LangPy,
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

	// srcdir is a shared scratch dir OUTSIDE both stacks' work dirs: the split
	// writes a chunk-input file there, so the path string recorded in
	// chunks.json is byte-identical across stacks (both runs write the same
	// bytes to the same path) while the staged copies under each chunk bundle
	// are what the parity diff compares.
	srcdir := filepath.Join(root, "shared")
	if err := os.MkdirAll(srcdir, 0o755); err != nil {
		t.Fatal(err)
	}

	env.argsDir = filepath.Join(root, "args")
	man := types.BuildManifest(prog)

	inParams, err := man.Params(s.Name, types.RoleIn)
	if err != nil {
		t.Fatal(err)
	}

	stageArgs := map[string]any{"value": 1.5, "srcdir": srcdir}
	if err := shim.WriteBundle(env.argsDir, stageArgs, inParams, man.Table()); err != nil {
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

	role := types.RoleMainOut
	outFlag := "outs"

	switch phase {
	case "split":
		role, outFlag = types.RoleChunkIn, ""
		extra = append(extra, "-o", "chunks.json", "-joinres", "joinres.json", "-chunkdir", ".")
	case "join":
		role = types.RoleOut
	}

	// The stacks differ only in the invocation head (mre additionally takes
	// the adapter's -shell and -lang, see stageCmd); the flag tail appended
	// below is identical for both.
	args := []string{phase, "-stagecode", e.stageCode, "-call", "CONF", "-mro", "pipeline.mro"}
	if stack == "mre" {
		args = []string{phase, "-shell", e.shell, "-stagecode", e.stageCode, "-lang", "py", "-call", "CONF", "-mro", "pipeline.mro"}
	}

	args = append(args, "-args", e.argsDir, "-types", e.typesFile, "-callable", e.callable, "-role", role,
		"-threads", "1", "-memgb", "1", "-work", ".")
	if outFlag != "" {
		args = append(args, "-o", outFlag)
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
	// Persist the combined output so cases can pin diagnostic-surface parity
	// (e.g. the ASSERT line both stacks must emit), not just exit codes.
	if werr := os.WriteFile(filepath.Join(dir, "combined.log"), out, 0o644); werr != nil {
		t.Fatal(werr)
	}

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

// mustContain asserts a produced artifact carries each want substring; it
// guards the conformance fixture's REACH (the byte-parity diff alone would
// pass vacuously if a trouble-spot value fell out of the fixture).
func mustContain(t *testing.T, path string, wants ...string) {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, w := range wants {
		if !strings.Contains(string(b), w) {
			t.Errorf("%s: missing %q (fixture no longer exercises this branch)", path, w)
		}
	}
}

// mustNotContain is the reach guard's negative half: it fails if path contains
// any of the given substrings, pinning that a branch was NOT taken.
func mustNotContain(t *testing.T, path string, unwanted ...string) {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, u := range unwanted {
		if strings.Contains(string(b), u) {
			t.Errorf("%s: unexpectedly contains %q", path, u)
		}
	}
}
