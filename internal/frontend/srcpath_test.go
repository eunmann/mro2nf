package frontend_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/frontend"
)

// TestSrcPathResolvedAgainstDeclaringFile guards #178: a relative stage `src`
// must resolve against the directory of the file that DECLARES the stage, not the
// entry .mro's directory. An @included stage in a subdirectory keeps its src next
// to the included file.
func TestSrcPathResolvedAgainstDeclaringFile(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The stage is declared in sub/stages.mro with a src relative to that file.
	if err := os.WriteFile(filepath.Join(dir, "sub", "stages.mro"), []byte(`
stage MK(
    out int  n,
    src py   "code.py",
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The entry pipeline includes it from the parent directory.
	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(`
@include "sub/stages.mro"

pipeline TOP(
    out int result,
)
{
    call MK()
    return (
        result = MK.n,
    )
}

call TOP()
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ast, err := frontend.Parse(filepath.Join(dir, "pipeline.mro"), []string{dir}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast, nil)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	mk := prog.Stages["MK"]
	if mk == nil {
		t.Fatal("stage MK not lowered")
	}

	want := filepath.Join(dir, "sub", "code.py") // next to the DECLARING file
	if mk.SrcPath != want {
		t.Errorf("MK.SrcPath = %q, want %q (resolved against the declaring file's dir, not the entry .mro's)", mk.SrcPath, want)
	}
}

// TestSrcPathFoundViaSearchPath guards the #178 review: when a stage's code is
// NOT next to its declaring file but is reachable via a -mropath search path,
// Martian's FindPath resolves it there (step 3). The declaring-dir-only resolve
// would bake a nonexistent path — a regression the search fallback prevents.
func TestSrcPathFoundViaSearchPath(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The stage is declared in sub/, but its code lives next to the ENTRY and is
	// found only via the -mropath search list (not colocated with the declaration).
	if err := os.WriteFile(filepath.Join(dir, "sub", "stages.mro"), []byte(`
stage MK(
    out int  n,
    src py   "code.py",
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.py"), []byte("# stage code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(`
@include "sub/stages.mro"

pipeline TOP(
    out int result,
)
{
    call MK()
    return (
        result = MK.n,
    )
}

call TOP()
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ast, err := frontend.Parse(filepath.Join(dir, "pipeline.mro"), []string{dir}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast, []string{dir})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	want := filepath.Join(dir, "code.py") // found via the search path, not sub/
	if got := prog.Stages["MK"].SrcPath; got != want {
		t.Errorf("MK.SrcPath = %q, want %q (resolved via -mropath search)", got, want)
	}
}

// TestSrcPathBareCommandStaysUnqualified guards real-toolkit pipelines like
// CellRanger, whose comp stages share a multi-call binary invoked as a bare
// command (`cr_lib martian <subcmd>`) that lives on PATH (in lib/bin), not in the
// mro tree. When such a command is not found as a file, Martian's FindPath returns
// it UNQUALIFIED so exec resolves it on PATH. Dir-joining it into the declaring
// file's directory (mro/rna/cr_lib) turns a PATH lookup into a nonexistent
// filesystem path — `fork/exec .../cr_lib: no such file or directory` at runtime.
// A bare command (no path separator) for a comp/exec stage must therefore pass
// through verbatim.
func TestSrcPathBareCommandStaysUnqualified(t *testing.T) {
	dir := t.TempDir()

	// A comp stage whose src is a bare command with subcommand args; the binary
	// is not present in the mro tree (it lives on PATH at run time).
	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(`
stage RUN(
    out  int   n,
    src  comp  "cr_lib martian do_thing",
)

pipeline TOP(
    out int result,
)
{
    call RUN()
    return (
        result = RUN.n,
    )
}

call TOP()
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ast, err := frontend.Parse(filepath.Join(dir, "pipeline.mro"), []string{dir}, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	prog, err := frontend.Lower(ast, []string{dir})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	if got := prog.Stages["RUN"].SrcPath; got != "cr_lib" {
		t.Errorf("RUN.SrcPath = %q, want %q (bare command kept unqualified for PATH resolution, not dir-joined)", got, "cr_lib")
	}
}
