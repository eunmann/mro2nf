package gen

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/test/corpus"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// stubTree is an invocation whose declarations span two included files:
// a filetype, a struct, a split stage with chunk params and resources, a
// comp stage, a map call, a disabled binding and a retain.
func stubTree() map[string]string {
	return map[string]string{
		"lib/types.mro": `filetype bam;

struct PAIR(
    bam      reads,
    int[]    counts,
    map<int> totals,
)
`,
		"lib/stages.mro": `@include "types.mro"

stage COUNT(
    in  PAIR   pair,
    in  int    n,
    out bool   skip,
    out bam[]  outs,
    src py     "stages/count",
) split (
    in  int    chunk,
    out string part,
) using (
    mem_gb   = 2,
    threads  = 2,
    volatile = strict,
) retain (
    outs,
)

stage SUM(
    in  bam  x,
    out PAIR p,
    src comp "bin/sum martian sum",
)
`,
		"pipeline.mro": `@include "lib/stages.mro"

pipeline TOP(
    in  PAIR   pair,
    out PAIR[] sums,
)
{
    call COUNT(
        pair = self.pair,
        n    = 3,
    )

    map call SUM(
        x = split COUNT.outs,
    ) using (
        disabled = COUNT.skip,
    )

    return (
        sums = SUM.p,
    )
}

call TOP(
    pair = null,
)
`,
	}
}

// TestWriteStubFixture checks the stub fixture of stubTree is one file
// Martian compiles with every stub in place, each stage's spec holding its
// compiled types, the structs every value may carry, the filetypes and the
// seed, and its declarations those of the tree.
func TestWriteStubFixture(t *testing.T) {
	src := writeStubTree(t, stubTree())
	dir := t.TempDir()

	if err := WriteStubFixture(dir, filepath.Join(src, "pipeline.mro"), []string{src}, 3); err != nil {
		t.Fatal(err)
	}

	mro, err := os.ReadFile(filepath.Join(dir, "pipeline.mro"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"split (", "mem_gb   = 2,", "retain (", "disabled = COUNT.skip,"} {
		if !strings.Contains(string(mro), want) {
			t.Errorf("pipeline.mro lacks %q:\n%s", want, mro)
		}
	}

	for line := range strings.Lines(string(mro)) {
		if strings.HasPrefix(line, "@include") || strings.Contains(line, "comp") {
			t.Errorf("pipeline.mro holds %q:\n%s", line, mro)
		}
	}

	if _, _, _, err := syntax.Compile(filepath.Join(dir, "pipeline.mro"), []string{dir}, true); err != nil {
		t.Errorf("the stub fixture does not compile with its stage code checked: %v", err)
	}

	pair := [][2]string{{"reads", "bam"}, {"counts", "int[]"}, {"totals", "map<int>"}}
	want := stubSpec{
		Name: "COUNT", Split: true, Seed: 3,
		In:        [][2]string{{"pair", "PAIR"}, {"n", "int"}},
		Out:       [][2]string{{"skip", "bool"}, {"outs", "bam[]"}},
		ChunkIn:   [][2]string{{"chunk", "int"}},
		ChunkOut:  [][2]string{{"part", "string"}},
		Structs:   map[string][][2]string{"PAIR": pair, "COUNT": {{"skip", "bool"}, {"outs", "bam[]"}}, "SUM": {{"p", "PAIR"}}, "TOP": {{"sums", "PAIR[]"}}},
		FileTypes: []string{"bam"},
	}

	if d := cmp.Diff(want, readStubSpec(t, filepath.Join(dir, "stages", "count"))); d != "" {
		t.Errorf("COUNT's spec (-want +got):\n%s", d)
	}
}

// TestWriteStubFixtureRefusesSharedStubDir checks two stages whose ids
// differ only in case, which would share a stub dir, are refused.
func TestWriteStubFixtureRefusesSharedStubDir(t *testing.T) {
	src := writeStubTree(t, map[string]string{"pipeline.mro": `
stage A_B(out int x, src py "a",)
stage a_b(out int x, src py "b",)

pipeline TOP(out int x,)
{
    call A_B()
    call a_b()
    return (x = A_B.x,)
}

call TOP()
`})

	err := WriteStubFixture(t.TempDir(), filepath.Join(src, "pipeline.mro"), []string{src}, 0)
	if !errors.Is(err, errStageDirCollision) {
		t.Errorf("WriteStubFixture = %v, want %v", err, errStageDirCollision)
	}
}

// TestWriteStubFixtureOfEveryCorpusInvocation writes the stub fixture of
// every corpus invocation Martian accepts, which WriteStubFixture holds to
// the invocation's call graph and declarations.
func TestWriteStubFixtureOfEveryCorpusInvocation(t *testing.T) {
	srcs, err := corpus.Sources(filepath.Join("..", "..", "testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := corpus.Load(t.TempDir(), srcs)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range corpus.Accepted(entries) {
		if err := WriteStubFixture(t.TempDir(), e.Mro, e.MroPath, 1); err != nil {
			t.Errorf("%s: %v", e.Name, err)
		}
	}
}

func writeStubTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func readStubSpec(t *testing.T, stageDir string) stubSpec {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(stageDir, "spec.json"))
	if err != nil {
		t.Fatal(err)
	}

	var spec stubSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	return spec
}
