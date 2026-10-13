package main

import (
	"encoding/base64"
	"encoding/json"
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

// stagedReads is stageMRO's entry file value, which its tasks stage.
const stagedReads = "s3://bucket/reads.txt"

// writeStagePipeline writes stageMRO, reading stagedReads, and its stage
// code to a new dir, and returns the .mro's path.
func writeStagePipeline(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), fmt.Sprintf(stageMRO, stagedReads))

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

// postedValues are the outs value records a post wrote to path, each as
// <output>:<its JSON>, space-separated.
func postedValues(t *testing.T, path string) string {
	t.Helper()

	recs := strings.Fields(readText(t, path))
	out := make([]string, 0, len(recs))

	for _, rec := range recs {
		field, enc, _ := strings.Cut(rec, ":")

		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatal(err)
		}

		var v struct {
			Value json.RawMessage `json:"value"`
		}

		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}

		out = append(out, field+":"+string(v.Value))
	}

	return strings.Join(out, " ")
}

// posted is a task's post flags, writing below dir.
func posted(dir string) []string {
	return []string{
		"-fields", dir + "/outs", "-field-values", dir + "/values", "-field-refs", dir + "/outs_refs",
		"-forks", dir + "/forks", "-fork-values", dir + "/fork_values", "-fork-refs", dir + "/fork_refs", "-gates", dir + "/gates",
	}
}

// unsplitTask is the mre stage call of a non-split stage's task in dir,
// its args step's own flags in in. Its flags name the task's files below
// the work dir, its cwd, as a task's own.
func unsplitTask(node, dir string, in ...string) []string {
	return slices.Concat([]string{"stage", "-phase", "main", "-node", node}, in,
		[]string{"-args", dir + "/args", "-work", dir}, posted(dir))
}

// splitTasks are the mre stage calls of a split stage's triad, each of
// which builds the args in its own dir with the args step's own flags
// given as in: its split, a main per chunk, each writing its outs into the
// join's chunkouts, and its join.
func splitTasks(node string, in ...string) [][]string {
	tasks := [][]string{slices.Concat([]string{"stage", "-phase", "split", "-node", node}, in,
		[]string{"-args", "sp/args", "-o", "sp/chunks", "-out-refs", "sp/chunk_refs", "-work", "sp"})}

	for _, c := range []string{"chnk0", "chnk1"} {
		tasks = append(tasks, slices.Concat([]string{"stage", "-phase", "main", "-node", node}, in,
			[]string{"-args", c + "/args", "-chunk", "sp/chunks/" + c, "-o", "jn/chunkouts/" + c, "-out-refs", c + "/chunkout_refs", "-work", c}))
	}

	return append(tasks, slices.Concat([]string{"stage", "-phase", "join", "-node", node}, in,
		[]string{"-args", "jn/args", "-chunks", "sp/chunks", "-chunkouts", "jn/chunkouts", "-work", "jn"}, posted("jn")))
}

// stageTasks are a run of stageMRO's enabled tasks, in dependency order,
// each fork of EACH given its fork record as a value.
func stageTasks(records []string) [][]string {
	tasks := append(splitTasks("P.SQ", "-leaves", "0"), unsplitTask("P.ADD", "add", "-values", "jn/values"), unsplitTask("P.CP", "cp", "-leaves", "0"))
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
	writeTestFile(t, filepath.Join("entry", "1", "reads.txt"), "7\n")

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

	for dir, want := range map[string]string{"jn/chunkouts/chnk0": `{"sq":4}`, "jn/chunkouts/chnk1": `{"sq":9}`} {
		if got := compactBundle(t, dir); got != want {
			t.Errorf("%s holds %s, want %s", dir, got, want)
		}
	}

	for values, want := range map[string]string{"jn/values": `total:13`, "add/values": `y:14`, "each0/values": `y:3`, "each1/values": `y:4`} {
		if got := postedValues(t, values); got != want {
			t.Errorf("%s holds %s, want %s", values, got, want)
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
		"-chunkouts", t.TempDir(), "-work", "jn2")
	if !errors.Is(err, errNoChunk) {
		t.Errorf("join without chunk outs: %v, want errNoChunk", err)
	}
}

// wantLinkedArgs requires every task of TestStageTasks that read the entry
// file to link it below its args dir, so no task writes a copy of it
// (#507), and to write no file there; and every job of SQ's triad to get
// the split's bindings, each its own link to the entry file.
func wantLinkedArgs(t *testing.T, tree map[string]string) {
	t.Helper()

	for _, task := range []string{"sp", "chnk0", "chnk1", "jn", "cp"} {
		if got := treeLeaf(t, tree, task+"/args/", "reads.txt"); !strings.HasPrefix(got, "-> ") {
			t.Errorf("%s's args holds reads as %q, want a link", task, got)
		}

		for rel, v := range tree {
			if strings.HasPrefix(rel, task+"/args/") && !strings.HasPrefix(v, "-> ") && v != "dir" && filepath.Base(rel) != ".mre-origin" {
				t.Errorf("%s's args dir holds the file %s", task, rel)
			}
		}
	}

	for task, meta := range map[string]string{"sp": "split", "chnk0": "chnk0", "chnk1": "chnk1", "jn": "join"} {
		var args struct {
			Xs    []int  `json:"xs"`
			Reads string `json:"reads"`
		}

		if err := json.Unmarshal([]byte(tree[task+"/"+meta+"/_args"]), &args); err != nil {
			t.Fatalf("%s's _args: %v", task, err)
		}

		reads, err := filepath.Abs(filepath.Join(task, "args", "f", "L0000", "reads.txt"))
		if err != nil {
			t.Fatal(err)
		}

		if d := cmp.Diff([]int{2, 3}, args.Xs); d != "" || args.Reads != reads {
			t.Errorf("%s's _args xs %v, reads %s, want the split's bindings, reads %s", task, args.Xs, args.Reads, reads)
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

// passMRO passes MK's file through PASS.
const passMRO = `filetype txt;

stage MK(
    out txt f,
    src exec "mk.sh",
)

stage PASS(
    in  txt x,
    out txt y,
    src exec "pass.sh",
)

pipeline P(
    out txt y,
)
{
    call MK()

    call PASS(
        x = MK.f,
    )

    return (
        y = PASS.y,
    )
}

call P()
`

// mkScript is MK's exec code: f is a file it writes, and passScript is
// PASS's: y is its x.
const (
	mkScript = `#!/bin/sh
meta=$2 files=$3
printf 'made' > "$files/f.txt"
echo "{\"f\":\"$files/f.txt\"}" > "$meta/_outs"
touch "$meta/_complete"
`
	passScript = `#!/bin/sh
meta=$2
echo "{\"y\":\"$(tr -d ' \n' < "$meta/_args" | sed 's/.*"x":"\([^"]*\)".*/\1/')\"}" > "$meta/_outs"
touch "$meta/_complete"
`
)

// TestStagePostNamesStagedFileByReference (#562): PASS's task stages MK's
// posted file as a driver stages it, below in/, and its post names it by
// reference, as the item the task staged it in and its ordinal dir there,
// rather than writing a copy of it into its outs.
func TestStagePostNamesStagedFileByReference(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), passMRO)

	for name, src := range map[string]string{"mk.sh": mkScript, "pass.sh": passScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(work)

	if _, err := mreIn(t, mro, unsplitTask("P.MK", "mk")...); err != nil {
		t.Fatalf("MK: %v", err)
	}

	if err := os.MkdirAll(filepath.Join("pass", "in"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(work, "mk", "outs", "f"), filepath.Join("pass", "in", "u1")); err != nil {
		t.Fatal(err)
	}

	if _, err := mreIn(t, mro, unsplitTask("P.PASS", "pass", "-in", "pass/in", "-refs", "pass/refs")...); err != nil {
		t.Fatalf("PASS: %v", err)
	}

	if got := readText(t, filepath.Join("pass", "outs_refs")); got != "outs/y\tpass/in/u1\tf/L0000" {
		t.Errorf("PASS's references %q, want its y in MK's f, staged as pass/in/u1", got)
	}

	if _, err := os.Lstat(filepath.Join("pass", "outs", "y", "f")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PASS posted a files dir (%v), want none", err)
	}
}

// fwdMRO forwards MK's file through FWD's split triad: the split into each
// chunk's def, each chunk into its outs, and the join into its own.
const fwdMRO = `filetype txt;

stage MK(
    out txt f,
    src exec "mk.sh",
)

stage FWD(
    in  txt x,
    out txt y,
    src exec "fwd.sh",
) split (
    in  txt c,
    out txt z,
)

pipeline P(
    out txt y,
)
{
    call MK()

    call FWD(
        x = MK.f,
    )

    return (
        y = FWD.y,
    )
}

call P()
`

// fwdScript is FWD's exec code: two chunks, each given x as c, each
// returning its c as z, and the join returning a chunk's z as y.
const fwdScript = `#!/bin/sh
meta=$2
args=$(tr -d ' \n' < "$meta/_args")
case $1 in
split) x=$(echo "$args" | sed 's/.*"x":"\([^"]*\)".*/\1/'); echo "{\"chunks\":[{\"c\":\"$x\"},{\"c\":\"$x\"}],\"join\":{}}" > "$meta/_stage_defs" ;;
main) c=$(echo "$args" | sed 's/.*"c":"\([^"]*\)".*/\1/'); echo "{\"z\":\"$c\"}" > "$meta/_outs" ;;
join) z=$(tr -d ' \n' < "$meta/_chunk_outs" | sed 's/.*"z":"\([^"]*\)".*/\1/'); echo "{\"y\":\"$z\"}" > "$meta/_outs" ;;
esac
touch "$meta/_complete"
`

// linkAll links each of links, a staged path, to its target below work,
// as Nextflow stages a task's inputs.
func linkAll(t *testing.T, work string, links map[string]string) {
	t.Helper()

	for at, target := range links {
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(filepath.Join(work, target), at); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStageSplitNamesStagedFileByReference: FWD's split stages MK's file
// and forwards it into each chunk's def, and each chunk into its outs: the
// split and each chunk name it by reference, as the item their task staged
// it in and its ordinal dir there, and write no copy of it, so its bytes
// are not emitted once per chunk. Each chunk and the join stage MK's
// output, as the split does, so the driver stages the file no second time
// by reference: each reads the reference as the file in that item, and the
// chunks' outs and the join's post name it there.
func TestStageSplitNamesStagedFileByReference(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), fwdMRO)

	for name, src := range map[string]string{"mk.sh": mkScript, "fwd.sh": fwdScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(work)

	if _, err := mreIn(t, mro, unsplitTask("P.MK", "mk")...); err != nil {
		t.Fatalf("MK: %v", err)
	}

	linkAll(t, work, map[string]string{"sp/in/u1": "mk/outs/f"})
	split := []string{"stage", "-phase", "split", "-node", "P.FWD", "-in", "sp/in", "-refs", "sp/refs", "-args", "sp/args", "-o", "sp/chunks", "-out-refs", "sp/chunk_refs", "-work", "sp"}

	if _, err := mreIn(t, mro, split...); err != nil {
		t.Fatalf("split: %v", err)
	}

	wantRefs(t, "sp/chunk_refs", "chunks/chnk0\tsp/in/u1\tf/L0000 chunks/chnk1\tsp/in/u1\tf/L0000", "sp/chunks/chnk0/f", "sp/chunks/chnk1/f")

	for _, c := range []string{"chnk0", "chnk1"} {
		linkAll(t, work, map[string]string{c + "/in/u1": "mk/outs/f", c + "/chunk/" + c: "sp/chunks/" + c})

		if _, err := mreIn(t, mro, "stage", "-phase", "main", "-node", "P.FWD", "-in", c+"/in", "-refs", c+"/refs", "-args", c+"/args",
			"-chunk", c+"/chunk/"+c, "-o", c+"/chunkout/"+c, "-out-refs", c+"/chunkout_refs", "-work", c); err != nil {
			t.Fatalf("%s: %v", c, err)
		}

		wantRefs(t, c+"/chunkout_refs", "chunkout/"+c+"\t"+c+"/in/u1\tf/L0000", c+"/chunkout/"+c+"/f")
	}

	linkAll(t, work, map[string]string{
		"jn/in/u1": "mk/outs/f", "jn/chunks/chnk0": "sp/chunks/chnk0", "jn/chunks/chnk1": "sp/chunks/chnk1",
		"jn/chunks/join.json": "sp/chunks/join.json", "jn/chunkouts/chnk0": "chnk0/chunkout/chnk0", "jn/chunkouts/chnk1": "chnk1/chunkout/chnk1",
	})

	if _, err := mreIn(t, mro, slices.Concat([]string{
		"stage", "-phase", "join", "-node", "P.FWD", "-in", "jn/in", "-refs", "jn/refs", "-args", "jn/args",
		"-chunks", "jn/chunks", "-chunkouts", "jn/chunkouts", "-work", "jn",
	}, posted("jn"))...); err != nil {
		t.Fatalf("join: %v", err)
	}

	wantRefs(t, "jn/outs_refs", "outs/y\tjn/in/u1\tf/L0000", "jn/outs/y/f")
}

// wantRefs requires the reference records a task wrote to path to be want,
// and each of copies, a files dir a bundle would hold a copy in, absent.
func wantRefs(t *testing.T, path, want string, copies ...string) {
	t.Helper()

	if d := cmp.Diff(want, readText(t, path)); d != "" {
		t.Errorf("%s (-want +got):\n%s", path, d)
	}

	for _, c := range copies {
		if _, err := os.Lstat(c); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists (%v), want the file named by reference", c, err)
		}
	}
}

// TestStageRefusesADisabledInstance: an instance its args resolve disabled
// fails its task with errNotEnabled, exit 1, naming it, before any job
// runs, and its task writes nothing.
func TestStageRefusesADisabledInstance(t *testing.T) {
	mro, dir := writeStagePipeline(t), t.TempDir()

	_, err := mreIn(t, mro, "stage", "-phase", "split", "-node", "P.OFF", "-args", filepath.Join(dir, "args"), "-o", filepath.Join(dir, "chunks"), "-work", dir)
	if !errors.Is(err, errNotEnabled) || exitCode(err) != 1 || !strings.Contains(err.Error(), "P.OFF") {
		t.Errorf("mre stage of a disabled instance: %v, exit %d, want errNotEnabled for P.OFF, exit 1", err, exitCode(err))
	}

	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a disabled instance's task wrote %v (%v)", entries, err)
	}
}

// TestStageExitCodes: a failed job exits 42 for an assert and 75 for a
// failure mrp retries, 1 for any other, and its task posts nothing.
func TestStageExitCodes(t *testing.T) {
	mro := writeStagePipeline(t)

	for node, want := range map[string]int{"P.ASSERT": job.AssertExitCode, "P.RETRY": job.TransientExitCode, "P.ERROR": 1} {
		dir := t.TempDir()

		_, err := mreIn(t, mro, slices.Concat([]string{
			"stage", "-phase", "main", "-node", node, "-args", filepath.Join(dir, "args"), "-work", dir,
		}, posted(dir))...)
		if err == nil || exitCode(err) != want {
			t.Errorf("%s: %v, exit %d, want exit %d", node, err, exitCode(err), want)
		}

		for _, f := range []string{"outs", "values", "outs_refs", "forks", "fork_values", "fork_refs", "gates"} {
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
