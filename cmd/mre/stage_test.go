package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/job"
	"github.com/google/go-cmp/cmp"
)

// stageMRO has a task of every shape: SQ's split triad, which also reads
// an entry file, ADD reading its total, CP reading an entry file, EACH
// forked over an entry value, OFF disabled, and FAIL failing as its how
// says.
const stageMRO = `filetype txt;

stage SQ(
    in  int[] xs,
    in  txt   reads,
    out int   total,
    src exec  "sq.sh",
) split (
    in  int   x,
    out int   sq,
)

stage ADD(
    in  int total,
    out int y,
    src exec "add.sh",
)

stage CP(
    in  txt reads,
    out txt copy,
    src exec "cp.sh",
)

stage FAIL(
    in  int how,
    src exec "fail.sh",
)

pipeline P(
    in  int[] xs,
    in  txt   reads,
    in  bool  off,
    out int   total,
)
{
    call SQ(
        xs    = self.xs,
        reads = self.reads,
    )

    call SQ as OFF(
        xs    = self.xs,
        reads = self.reads,
    ) using (
        disabled = self.off,
    )

    call ADD(
        total = SQ.total,
    )

    map call ADD as EACH(
        total = split self.xs,
    )

    call CP(
        reads = self.reads,
    )

    call FAIL as ASSERT(
        how = 1,
    )

    call FAIL as RETRY(
        how = 2,
    )

    call FAIL as ERROR(
        how = 3,
    )

    return (
        total = SQ.total,
    )
}

call P(
    xs    = [2, 3],
    reads = %q,
    off   = true,
)
`

// addScript is ADD's exec code: y is its total plus 1.
const addScript = `#!/bin/sh
meta=$2
v=$(tr -d ' \n' < "$meta/_args" | sed 's/.*"total":\([0-9]*\).*/\1/')
echo "{\"y\":$((v + 1))}" > "$meta/_outs"
touch "$meta/_complete"
`

// cpScript is CP's exec code: copy is a copy of reads in its files dir.
const cpScript = `#!/bin/sh
meta=$2 files=$3
cp "$(tr -d ' \n' < "$meta/_args" | sed 's/.*"reads":"\([^"]*\)".*/\1/')" "$files/copy.txt"
echo "{\"copy\":\"$files/copy.txt\"}" > "$meta/_outs"
touch "$meta/_complete"
`

// failScript is FAIL's exec code: how 1 asserts, 2 fails as mrp retries
// (ENOMEM), any other fails as it does not.
const failScript = `#!/bin/sh
meta=$2
case $(tr -d ' \n' < "$meta/_args" | sed 's/.*"how":\([0-9]*\).*/\1/') in
1) printf 'nope' > "$meta/_assert" ;;
2) printf 'error: [Errno 12] Cannot allocate memory' > "$meta/_errors" ;;
*) printf 'Traceback' > "$meta/_errors" ;;
esac
`

// writeStagePipeline writes stageMRO and its stage code to a new dir, the
// entry file value it reads beside them, and returns the .mro's path.
func writeStagePipeline(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	reads := writeTestFile(t, filepath.Join(dir, "reads.txt"), "7\n")
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), fmt.Sprintf(stageMRO, reads))

	for name, src := range map[string]string{"sq.sh": sqScript, "add.sh": addScript, "cp.sh": cpScript, "fail.sh": failScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	return mro
}

// mreIn runs one mre subcommand against the pipeline at mro.
func mreIn(t *testing.T, mro string, argv ...string) (string, error) {
	t.Helper()

	var err error

	out := captureStdout(t, func() { err = run(t.Context(), append([]string{argv[0], "-entry", mro}, argv[1:]...)) })

	return out, err
}

// posted is a task's post flags, writing below dir.
func posted(dir string) []string {
	return []string{"-fields", dir + "/outs", "-forks", dir + "/forks", "-gates", dir + "/gates"}
}

// unsplitTask is the mre stage call of a non-split stage's task in dir,
// its args step's own flags in in. Its flags name the task's files below
// the work dir, its cwd, as a task's own.
func unsplitTask(node, dir string, in ...string) []string {
	return slices.Concat([]string{"stage", "-phase", "main", "-node", node}, in,
		[]string{"-args", dir + "/args", "-o", dir + "/stage_outs", "-work", dir}, posted(dir))
}

// splitTasks are the mre stage calls of a split stage's triad, each of
// which builds the args bundle in its own dir with the args step's own
// flags given as in: its split, a main per chunk, each writing its outs
// into the join's chunkouts, and its join.
func splitTasks(node string, in ...string) [][]string {
	tasks := [][]string{slices.Concat([]string{"stage", "-phase", "split", "-node", node}, in,
		[]string{"-args", "sp/args", "-o", "sp/chunks", "-work", "sp"})}

	for _, c := range []string{"chnk0", "chnk1"} {
		tasks = append(tasks, slices.Concat([]string{"stage", "-phase", "main", "-node", node}, in,
			[]string{"-args", c + "/args", "-chunk", "sp/chunks/" + c, "-o", "jn/chunkouts/" + c, "-work", c}))
	}

	return append(tasks, slices.Concat([]string{"stage", "-phase", "join", "-node", node}, in,
		[]string{"-args", "jn/args", "-chunks", "sp/chunks", "-chunkouts", "jn/chunkouts", "-o", "jn/stage_outs", "-work", "jn"}, posted("jn")))
}

// stageTasks are a run of stageMRO's enabled tasks, in dependency order,
// each fork of EACH given its fork record as a value.
func stageTasks(records []string) [][]string {
	tasks := append(splitTasks("P.SQ", "-leaves", "0"), unsplitTask("P.ADD", "add", "-in", "jn/outs/total"), unsplitTask("P.CP", "cp", "-leaves", "0"))
	for i, rec := range records {
		tasks = append(tasks, unsplitTask("P.EACH", "each"+strconv.Itoa(i), "-fork", rec))
	}

	return tasks
}

// stageWork makes a new task work dir at work and enters it, as a run's
// tasks find theirs: the entry file staged as entry/1/, and the forks of
// EACH PLAN fixed. It returns their fork records.
func stageWork(t *testing.T, mro, work string) []string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(work, "entry", "1"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(work)

	if err := os.Symlink(filepath.Join(filepath.Dir(mro), "reads.txt"), filepath.Join("entry", "1", "reads.txt")); err != nil {
		t.Fatal(err)
	}

	entryForks(t, graphFlags{entry: mro}, "plan")

	elems, err := filepath.Glob(filepath.Join("plan", "P.EACH", "*"))
	if err != nil || len(elems) != 2 {
		t.Fatalf("entry forks of EACH: %v %v", elems, err)
	}

	records := make([]string, len(elems))
	for i, el := range elems {
		records[i] = base64.StdEncoding.EncodeToString([]byte(readText(t, filepath.Join(el, "fork.json"))))
	}

	return records
}

// snapshot is the tree below dir: each file's content, each link's target.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()

	tree := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			tree[rel] = "-> " + target

			return err
		case d.IsDir():
			tree[rel] = "dir"
		default:
			raw, err := os.ReadFile(path)
			tree[rel] = string(raw)

			return err
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return tree
}

// TestStageTasks: each task of a run of stageMRO, one mre stage call,
// writes what its consumers stage, for every task shape: SQ's split triad,
// each chunk's outs and the total the join posts; ADD, a non-split stage,
// reading that total; CP, reading the entry file through its args bundle's
// link; and each fork of the map call EACH, given its fork record as a
// value. Each task runs its job in its own work dir, and a join missing a
// chunk's outs runs none.
func TestStageTasks(t *testing.T) {
	mro, work := writeStagePipeline(t), t.TempDir()

	for _, argv := range stageTasks(stageWork(t, mro, work)) {
		if _, err := mreIn(t, mro, argv...); err != nil {
			t.Fatalf("mre %v: %v", argv, err)
		}
	}

	for dir, want := range map[string]string{
		"jn/chunkouts/chnk0": `{"sq":4}`, "jn/chunkouts/chnk1": `{"sq":9}`, "jn/outs/total": `{"total":13}`,
		"add/outs/y": `{"y":14}`, "each0/outs/y": `{"y":3}`, "each1/outs/y": `{"y":4}`,
	} {
		if got := compactBundle(t, dir); got != want {
			t.Errorf("%s holds %s, want %s", dir, got, want)
		}
	}

	for meta, stage := range map[string]string{
		"sp/split": "SQ", "chnk0/chnk0": "SQ", "chnk1/chnk1": "SQ", "jn/join": "SQ",
		"add/chnk0": "ADD", "cp/chnk0": "CP", "each0/chnk0": "ADD", "each1/chnk0": "ADD",
	} {
		if got := readText(t, filepath.Join(meta, stageNameFile)); got != stage {
			t.Errorf("the job in %s ran %q, want %s", meta, got, stage)
		}
	}

	tree := snapshot(t, work)

	if got := treeLeaf(t, tree, "cp/outs/copy/", "copy.txt"); got != "7\n" {
		t.Errorf("CP's copy holds %q, want the entry file's 7", got)
	}

	wantLinkedArgs(t, tree)

	_, err := mreIn(t, mro, "stage", "-phase", "join", "-node", "P.SQ", "-leaves", "0", "-args", "jn2/args", "-chunks", "sp/chunks",
		"-chunkouts", t.TempDir(), "-o", "jn2/stage_outs", "-work", "jn2")
	if !errors.Is(err, errNoChunk) {
		t.Errorf("join without chunk outs: %v, want errNoChunk", err)
	}
}

// wantLinkedArgs requires every task of TestStageTasks that read the entry
// file to link it in its args bundle, so no task writes a copy of it (#507),
// and every task of SQ's triad to build the split's args payload.
func wantLinkedArgs(t *testing.T, tree map[string]string) {
	t.Helper()

	for _, task := range []string{"sp", "chnk0", "chnk1", "jn", "cp"} {
		if got := treeLeaf(t, tree, task+"/args/", "reads.txt"); !strings.HasPrefix(got, "-> ") {
			t.Errorf("%s's args holds reads as %q, want a link", task, got)
		}
	}

	split := tree["sp/args/data.json"]
	if split == "" {
		t.Fatal("the split wrote no args payload")
	}

	for _, task := range []string{"chnk0", "chnk1", "jn"} {
		if d := cmp.Diff(split, tree[task+"/args/data.json"]); d != "" {
			t.Errorf("%s's args payload is not the split's (-split +%s):\n%s", task, task, d)
		}
	}
}

// treeLeaf is the snapshot entry of the one file named name below prefix.
func treeLeaf(t *testing.T, tree map[string]string, prefix, name string) string {
	t.Helper()

	var leaves []string

	for rel, v := range tree {
		if strings.HasPrefix(rel, prefix) && filepath.Base(rel) == name {
			leaves = append(leaves, v)
		}
	}

	if len(leaves) != 1 {
		t.Fatalf("%s holds %d %s leaves, want 1", prefix, len(leaves), name)
	}

	return leaves[0]
}

// TestStageRefusesADisabledInstance: an instance its args resolve disabled
// fails its task with errNotEnabled, exit 1, before any job runs, and its
// args bundle carries only its stamp, which records it disabled.
func TestStageRefusesADisabledInstance(t *testing.T) {
	mro, dir := writeStagePipeline(t), t.TempDir()
	args := filepath.Join(dir, "args")

	_, err := mreIn(t, mro, "stage", "-phase", "split", "-node", "P.OFF", "-args", args, "-o", filepath.Join(dir, "chunks"), "-work", dir)
	if !errors.Is(err, errNotEnabled) || exitCode(err) != 1 {
		t.Errorf("mre stage of a disabled instance: %v, exit %d, want errNotEnabled, exit 1", err, exitCode(err))
	}

	for _, f := range []string{job.PhaseSplit, "chunks"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a disabled instance's task wrote %s: %v", f, err)
		}
	}

	g, _, err := (&graphFlags{entry: mro}).load()
	if err != nil {
		t.Fatal(err)
	}

	if inst, err := g.ReadInstance(args); err != nil || !inst.Disabled || inst.Node != "P.OFF" {
		t.Errorf("the args stamp: %+v, %v, want P.OFF disabled", inst, err)
	}
}

// TestStageExitCodes: a failed job exits 42 for an assert and 75 for a
// failure mrp retries, 1 for any other, and its task posts nothing.
func TestStageExitCodes(t *testing.T) {
	mro := writeStagePipeline(t)

	for node, want := range map[string]int{"P.ASSERT": job.AssertExitCode, "P.RETRY": job.TransientExitCode, "P.ERROR": 1} {
		dir := t.TempDir()

		_, err := mreIn(t, mro, slices.Concat([]string{
			"stage", "-phase", "main", "-node", node, "-args", filepath.Join(dir, "args"),
			"-o", filepath.Join(dir, "stage_outs"), "-work", dir,
		}, posted(dir))...)
		if err == nil || exitCode(err) != want {
			t.Errorf("%s: %v, exit %d, want exit %d", node, err, exitCode(err), want)
		}

		for _, f := range []string{"stage_outs", "outs", "forks", "gates"} {
			if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: a failed job's task wrote %s: %v", node, f, err)
			}
		}
	}
}

// TestStageRefusals: mre stage refuses a phase that is none of split,
// main and join, and a main of a node the call graph has not, before it
// writes anything.
func TestStageRefusals(t *testing.T) {
	mro := writeStagePipeline(t)

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"phase", []string{"-phase", "chunk", "-node", "P.SQ"}},
		{"node", []string{"-phase", "main", "-node", "P.NONE"}},
	} {
		dir := t.TempDir()
		if _, err := mreIn(t, mro, slices.Concat([]string{"stage", "-args", filepath.Join(dir, "args"), "-work", dir}, tc.argv)...); err == nil {
			t.Errorf("%s: mre stage %v succeeded", tc.name, tc.argv)
		}

		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("%s: mre stage wrote %v (%v)", tc.name, entries, err)
		}
	}
}
