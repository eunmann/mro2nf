package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// sqMRO splits SQ over xs, one chunk per element; OFF is the same stage
// disabled, and ONE a py stage with no split.
const sqMRO = `stage SQ(
    in  int[] xs,
    out int   total,
    src exec  "sq.sh",
) split (
    in  int   x,
    out int   sq,
)

stage ONE(
    in  int   x,
    out int   y,
    src py    "one",
)

pipeline P(
    in  int[] xs,
    in  bool  off,
    out int   total,
)
{
    call SQ(
        xs = self.xs,
    )

    call SQ as OFF(
        xs = self.xs,
    ) using (
        disabled = self.off,
    )

    call ONE(
        x = 1,
    )

    return (
        total = SQ.total,
    )
}

call P(
    xs  = [2, 3],
    off = true,
)
`

// sqScript is SQ's exec code: split makes a chunk per element, main squares
// its element, and join sums the chunks' squares.
const sqScript = `#!/bin/sh
meta=$2
case $1 in
split) echo '{"chunks":[{"x":2},{"x":3}],"join":{}}' > "$meta/_stage_defs" ;;
main) x=$(tr -d ' \n' < "$meta/_args" | sed 's/.*"x":\([0-9]*\).*/\1/'); echo "{\"sq\":$((x * x))}" > "$meta/_outs" ;;
join) echo "{\"total\":$(($(tr -d ' \n' < "$meta/_chunk_outs" | grep -o '"sq":[0-9]*' | cut -d: -f2 | paste -sd+)))}" > "$meta/_outs" ;;
esac
touch "$meta/_complete"
`

// runSQ runs one mre subcommand against the sq pipeline in dir.
func runSQ(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()

	argv := append([]string{args[0], "-entry", filepath.Join(dir, "pipeline.mro")}, args[1:]...)

	var err error

	out := captureStdout(t, func() { err = run(t.Context(), argv) })

	return out, err
}

func writeSQ(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pipeline.mro"), []byte(sqMRO), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "sq.sh"), []byte(sqScript), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(dir, "one"), 0o700); err != nil {
		t.Fatal(err)
	}

	return dir
}

// allocation is the resources a job's _jobinfo or _args record.
type allocation struct {
	Threads    float64 `json:"threads"`
	MemGB      float64 `json:"memGB"`
	VMemGB     float64 `json:"vmemGB"`
	ArgThreads float64 `json:"__threads"`
	ArgMemGB   float64 `json:"__mem_gb"`
	ArgVMemGB  float64 `json:"__vmem_gb"`
}

func readAllocation(t *testing.T, path string) allocation {
	t.Helper()

	var a allocation
	if err := json.Unmarshal([]byte(readText(t, path)), &a); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	return a
}

// TestStageUnderOverrides: under -overrides, each job of a split stage gets
// its phase's overridden resources in its _jobinfo, a chunk's in its _args
// too, and the split schedules its chunks and join by them; an overrides
// key mrp does not know fails the job before any stage code runs.
func TestStageUnderOverrides(t *testing.T) {
	dir, work := writeSQ(t), t.TempDir()
	args, chunks := filepath.Join(work, "args"), filepath.Join(work, "chunks")
	over := writeTestFile(t, filepath.Join(work, "overrides.json"),
		`{"P": {"chunk.mem_gb": 2}, "P.SQ": {"split.threads": 2, "chunk.mem_gb": 3, "join.vmem_gb": 9}}`)

	phases := []struct {
		phase, meta string
		argv        []string
		want        allocation
	}{
		{"split", "split", []string{"-o", chunks}, allocation{Threads: 2, MemGB: 1, VMemGB: 4}},
		{"main", "chnk0", []string{"-chunk", filepath.Join(chunks, "chnk0"), "-o", filepath.Join(work, "chunkouts", "chnk0")}, allocation{Threads: 1, MemGB: 3, VMemGB: 6}},
		{"main", "chnk1", []string{"-chunk", filepath.Join(chunks, "chnk1"), "-o", filepath.Join(work, "chunkouts", "chnk1")}, allocation{Threads: 1, MemGB: 3, VMemGB: 6}},
		{"join", "join", []string{
			"-chunks", chunks, "-chunkouts", filepath.Join(work, "chunkouts"), "-o", filepath.Join(work, "outs"), "-forks", filepath.Join(work, "forks"),
		}, allocation{Threads: 1, MemGB: 1, VMemGB: 9}},
	}

	for _, p := range phases {
		jw := filepath.Join(work, p.meta+"-work")
		if _, err := runSQ(t, dir, append([]string{"stage", "-phase", p.phase, "-node", "P.SQ", "-args", args, "-overrides", over, "-work", jw}, p.argv...)...); err != nil {
			t.Fatalf("%s: %v", p.meta, err)
		}

		if got := readAllocation(t, filepath.Join(jw, p.meta, "_jobinfo")); got != p.want {
			t.Errorf("%s _jobinfo resources %+v, want %+v", p.meta, got, p.want)
		}
	}

	if got, want := readText(t, filepath.Join(chunks, resourcesTable)), "chnk0\t1\t3\t\nchnk1\t1\t3\t\njoin\t1\t1\t\n"; got != want {
		t.Errorf("resources table %q, want %q", got, want)
	}

	if got := readAllocation(t, filepath.Join(work, "chnk0-work", "chnk0", "_args")); got != (allocation{ArgThreads: 1, ArgMemGB: 3, ArgVMemGB: 6}) {
		t.Errorf("chunk _args resources %+v, want 1 thread, 3 GB, 6 GB vmem", got)
	}

	bad := writeTestFile(t, filepath.Join(work, "bad.json"), `{"P.SQ": {"threads": 2}}`)
	if _, err := runSQ(t, dir, "stage", "-phase", "split", "-node", "P.SQ", "-args", filepath.Join(t.TempDir(), "args"), "-overrides", bad, "-o", t.TempDir(), "-work", t.TempDir()); err == nil || !strings.Contains(err.Error(), `unknown field "threads"`) {
		t.Errorf("stage under an unknown key: %v, want it refused", err)
	}
}

// TestStageJobRefusals: mre stage refuses, before it runs any stage code,
// an instance its args resolve disabled, and a later phase's job of one,
// which the args bundle's stamp records disabled; a phase the stage has no
// job for; an unknown phase; and a py job whose work dir cannot hold the
// adapters mre embeds. Each case's task reads the args bundle the cases
// before it built.
func TestStageJobRefusals(t *testing.T) {
	dir, work := writeSQ(t), t.TempDir()
	off, one := filepath.Join(work, "off"), filepath.Join(work, "one")

	file := filepath.Join(work, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		argv []string
		want error
	}{
		{"disabled", []string{"-phase", "split", "-node", "P.OFF", "-args", off}, errNotEnabled},
		{"disabled join", []string{"-phase", "join", "-node", "P.OFF", "-args", off}, errDisabledJob},
		{"phase", []string{"-phase", "chunk", "-node", "P.SQ", "-args", filepath.Join(work, "args")}, errPhase},
		{"split", []string{"-phase", "split", "-node", "P.ONE", "-args", one, "-mrjob", "/opt/mrjob"}, errNotSplit},
		{"join", []string{"-phase", "join", "-node", "P.ONE", "-args", one, "-mrjob", "/opt/mrjob"}, errNotSplit},
	} {
		argv := append([]string{"stage", "-o", filepath.Join(work, tc.name), "-work", filepath.Join(work, tc.name+"-work")}, tc.argv...)
		if _, err := runSQ(t, dir, argv...); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}

	_, err := runSQ(t, dir, "stage", "-phase", "main", "-node", "P.ONE", "-args", filepath.Join(work, "py"), "-o", filepath.Join(work, "adapters"), "-work", file)
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "extract adapters") {
		t.Errorf("py main over a file work dir: %v, want the adapters' extraction's ENOTDIR", err)
	}
}
