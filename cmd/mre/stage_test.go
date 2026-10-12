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

// stageTask is one stage task: the args, run and post calls stage.nf made
// before mre stage, and the one mre stage call. Its flags name the task's
// files below the work dir, its cwd, as a task's own.
type stageTask struct {
	name  string
	steps [][]string
	stage []string
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

// unsplitTask is a non-split stage's task in dir: args, linked, main and
// post, its args step's own flags in in.
func unsplitTask(name, node, dir string, in ...string) stageTask {
	job := []string{"-args", dir + "/args", "-o", dir + "/stage_outs", "-work", dir}

	return stageTask{
		name: name,
		steps: [][]string{
			slices.Concat([]string{"args", "-node", node, "-o", dir + "/args", "-link"}, in),
			slices.Concat([]string{"run", "-phase", "main"}, job),
			slices.Concat([]string{"post", "-args", dir + "/args", "-outs", dir + "/stage_outs"}, posted(dir)),
		},
		stage: slices.Concat([]string{"stage", "-phase", "main", "-node", node}, in, job, posted(dir)),
	}
}

// splitTasks are a split stage's triad: its split, which builds the args
// bundle, copied, with the args step's own flags given as in, a main per
// chunk, each writing its outs into the join's chunkouts, and its join.
func splitTasks(node string, in ...string) []stageTask {
	split := []string{"-args", "sp/args", "-o", "sp/chunks", "-work", "sp"}
	tasks := []stageTask{{
		name: "split",
		steps: [][]string{
			slices.Concat([]string{"args", "-node", node, "-o", "sp/args"}, in),
			slices.Concat([]string{"run", "-phase", "split"}, split),
		},
		stage: slices.Concat([]string{"stage", "-phase", "split", "-node", node}, in, split),
	}}

	for _, c := range []string{"chnk0", "chnk1"} {
		main := []string{"-args", "sp/args", "-chunk", "sp/chunks/" + c, "-o", "jn/chunkouts/" + c, "-work", c}
		tasks = append(tasks, stageTask{
			name:  c,
			steps: [][]string{slices.Concat([]string{"run", "-phase", "main"}, main)},
			stage: slices.Concat([]string{"stage", "-phase", "main", "-node", node}, main),
		})
	}

	join := []string{"-args", "sp/args", "-chunks", "sp/chunks", "-chunkouts", "jn/chunkouts", "-o", "jn/stage_outs", "-work", "jn"}

	return append(tasks, stageTask{
		name: "join",
		steps: [][]string{
			slices.Concat([]string{"run", "-phase", "join"}, join),
			slices.Concat([]string{"post", "-args", "sp/args", "-outs", "jn/stage_outs"}, posted("jn")),
		},
		stage: slices.Concat([]string{"stage", "-phase", "join", "-node", node}, join, posted("jn")),
	})
}

// stageTasks are a run of stageMRO's enabled tasks, in dependency order,
// each fork of EACH given its fork record as a value.
func stageTasks(t *testing.T, records []string) []stageTask {
	t.Helper()

	tasks := append(splitTasks("P.SQ", "-leaves", "0"), unsplitTask("non-split", "P.ADD", "add", "-in", "jn/outs/total"), unsplitTask("entry file", "P.CP", "cp", "-leaves", "0"))
	for i, rec := range records {
		tasks = append(tasks, unsplitTask("fork "+strconv.Itoa(i), "P.EACH", "each"+strconv.Itoa(i), "-fork", rec))
	}

	return tasks
}

// stageWork makes a new task work dir at work and enters it, as a run's
// tasks find theirs: the entry file staged as entry/1/, and the forks of
// EACH PLAN fixed. It returns their fork records.
func stageWork(t *testing.T, mro, work string) []string {
	t.Helper()

	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(work, "entry", "1"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(work)

	if err := os.Symlink(filepath.Join(filepath.Dir(mro), "reads.txt"), filepath.Join("entry", "1", "reads.txt")); err != nil {
		t.Fatal(err)
	}

	if _, err := mreIn(t, mro, "post", "-entry-forks", "-forks", "plan"); err != nil {
		t.Fatalf("post -entry-forks: %v", err)
	}

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

// TestStageMatchesArgsRunPost: mre stage writes the same tree, byte for
// byte, as the args, run and post calls a task made before it, for every
// task shape: a split triad, a non-split stage reading another's outs, one
// reading an entry file, the forks of a map call and a disabled instance,
// which mre args reports and mre stage refuses before it runs any job.
func TestStageMatchesArgsRunPost(t *testing.T) {
	mro, work := writeStagePipeline(t), filepath.Join(t.TempDir(), "work")

	trees := make([]map[string]string, 2)

	for i, oneCall := range []bool{false, true} {
		records := stageWork(t, mro, work)

		for _, task := range stageTasks(t, records) {
			runStageTask(t, mro, task, oneCall)
		}

		state, err := mreIn(t, mro, "args", "-node", "P.OFF", "-o", "off/args")
		if oneCall {
			_, err = mreIn(t, mro, "stage", "-phase", "split", "-node", "P.OFF", "-args", "off/args", "-o", "off/chunks", "-work", "off")
			if !errors.Is(err, errNotEnabled) || exitCode(err) != 1 {
				t.Errorf("mre stage of a disabled instance: %v, exit %d, want errNotEnabled, exit 1", err, exitCode(err))
			}
		} else if err != nil || state != "disabled\n" {
			t.Errorf("mre args of OFF: %q, %v, want disabled", state, err)
		}

		if _, err := os.Stat(filepath.Join(work, "off", job.PhaseSplit)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a disabled instance has a job dir: %v", err)
		}

		trees[i] = snapshot(t, work)
	}

	if d := cmp.Diff(trees[0], trees[1]); d != "" {
		t.Errorf("task trees differ (-args/run/post +stage):\n%s", d)
	}

	if got := argsLeaf(t, trees[1], "sp"); got != "7\n" {
		t.Errorf("the split's args holds reads as %q, want a copy, as the chunks and join stage it", got)
	}

	if got := argsLeaf(t, trees[1], "cp"); !strings.HasPrefix(got, "-> ") {
		t.Errorf("the non-split main's args holds reads as %q, want a link", got)
	}

	if got := compactBundle(t, filepath.Join(work, "add", "outs", "y")); got != `{"y":14}` {
		t.Errorf("ADD's y %s, want SQ's total 13 plus 1", got)
	}
}

// argsLeaf is the snapshot entry of the reads file leaf in the args bundle
// of the task in dir.
func argsLeaf(t *testing.T, tree map[string]string, dir string) string {
	t.Helper()

	var leaves []string

	for rel, v := range tree {
		if strings.HasPrefix(rel, dir+"/args/") && filepath.Base(rel) == "reads.txt" {
			leaves = append(leaves, v)
		}
	}

	if len(leaves) != 1 {
		t.Fatalf("%s/args holds %d reads.txt leaves, want 1", dir, len(leaves))
	}

	return leaves[0]
}

func runStageTask(t *testing.T, mro string, task stageTask, oneCall bool) {
	t.Helper()

	calls := task.steps
	if oneCall {
		calls = [][]string{task.stage}
	}

	for _, argv := range calls {
		if _, err := mreIn(t, mro, argv...); err != nil {
			t.Fatalf("%s: mre %v: %v", task.name, argv, err)
		}
	}
}

// TestStageExitCodes: a failed job exits as mre run's does, 42 for an
// assert and 75 for a failure mrp retries, 1 for any other, and its task
// posts nothing.
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
