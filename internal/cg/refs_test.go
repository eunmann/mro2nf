package cg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
)

// refMRO passes MAKE's file through PASS, and READ reads it both through
// PASS and from MAKE, as do the pipeline outs.
const refMRO = `filetype txt;

stage MAKE(
    out txt made,
    src py  "make",
)

stage PASS(
    in  txt x,
    out txt y,
    src py  "pass",
)

stage READ(
    in  txt y,
    in  txt made,
    src py  "read",
)

pipeline P(
    out txt passed,
    out txt made,
)
{
    call MAKE()

    call PASS(
        x = MAKE.made,
    )

    call READ(
        y    = PASS.y,
        made = MAKE.made,
    )

    return (
        passed = PASS.y,
        made   = MAKE.made,
    )
}

call P()
`

// refRun is a run of refMRO as on an object store: each task runs in a
// scratch dir, stages copies of its inputs, and leaves its posted outs in
// its work dir. With refs, each post names a file its task staged by
// reference, which the run resolves, as the driver does, to where its
// producer wrote it (itemRefs).
type refRun struct {
	t        *testing.T
	g        *Graph
	root     string
	refs     bool
	itemRefs map[string][]string
}

func newRefRun(t *testing.T, refs bool) *refRun {
	t.Helper()

	root := t.TempDir()
	mro := writeFileOf(t, filepath.Join(root, "pipeline.mro"), refMRO)

	g, err := Load(mro, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	return &refRun{t: t, g: g, root: root, refs: refs, itemRefs: map[string][]string{}}
}

// stage copies the item dirs and the ordinal dir of each file they name
// by reference that lies in none of them into the task dir, cwd, as
// in/u<i> and refs/<i>/f/<ordinal>, and returns those files once each, as
// the driver stages them (refsOf).
func (r *refRun) stage(items []string) []string {
	r.t.Helper()

	for _, d := range []string{"in", "refs"} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			r.t.Fatal(err)
		}
	}

	var refs []string

	for i, it := range items {
		if err := shim.CopyTree(it, filepath.Join("in", "u"+strconv.Itoa(i+1))); err != nil {
			r.t.Fatal(err)
		}

		for _, o := range r.itemRefs[it] {
			if !slices.Contains(refs, o) && !slices.Contains(items, filepath.Dir(filepath.Dir(o))) {
				refs = append(refs, o)
			}
		}
	}

	for i, o := range refs {
		at := filepath.Join("refs", strconv.Itoa(i+1), "f")
		if err := os.MkdirAll(at, dirPerm); err != nil {
			r.t.Fatal(err)
		}

		if err := shim.CopyTree(o, filepath.Join(at, filepath.Base(o))); err != nil {
			r.t.Fatal(err)
		}
	}

	return refs
}

// task runs node's task on the item dirs it stages: its args, the outs its
// stage returns from them, posted output by output, which it leaves in its
// work dir, whose items it returns, and the args.
func (r *refRun) task(node string, items []string, stage func(args map[string]string, files string) map[string]any) ([]string, string) {
	r.t.Helper()

	scratch := filepath.Join(r.root, "scratch", node)
	files := writeFileOf(r.t, filepath.Join(scratch, "chnk0", "files", ".keep"), "")

	r.t.Chdir(scratch)

	refs := r.stage(items)

	staged, err := shim.NewStaged("in", "refs")
	if err != nil {
		r.t.Fatal(err)
	}

	leaves, err := staged.Leaves()
	if err != nil {
		r.t.Fatal(err)
	}

	inst, args, err := r.g.WriteArgs(ArgsSpec{Node: node, In: []string{"in"}, Refs: leaves, Out: "args"})
	if err != nil {
		r.t.Fatal(err)
	}

	paths := map[string]string{}

	for k, v := range args {
		var p string
		if json.Unmarshal(v, &p) == nil {
			paths[k] = p
		}
	}

	r.post(inst, scratch, stage(paths, filepath.Dir(files)))

	work := filepath.Join(r.root, "work", node)
	if err := shim.CopyTree(filepath.Join(scratch, "outs"), filepath.Join(work, "outs")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.t.Fatal(err)
	}

	r.resolve(work, items, refs)

	posted, err := filepath.Glob(filepath.Join(work, "outs", "*"))
	if err != nil {
		r.t.Fatal(err)
	}

	return posted, strings.ReplaceAll(compactArgs(r.t, args), scratch, "<task>")
}

func (r *refRun) post(inst Instance, scratch string, outs map[string]any) {
	r.t.Helper()

	conf, err := shim.ConfineStage(scratch, "chnk0", true, shim.Roots{})
	if err != nil {
		r.t.Fatal(err)
	}

	var staged shim.Staged
	if r.refs {
		if staged, err = shim.NewStaged("in", "refs"); err != nil {
			r.t.Fatal(err)
		}
	}

	m := core.LazyArgumentMap{}

	for k, v := range outs {
		if m[k], err = json.Marshal(v); err != nil {
			r.t.Fatal(err)
		}
	}

	err = r.g.Post(PostSpec{
		Instance: inst, Outs: m, Opts: shim.Opts{Confine: conf}, Staged: staged,
		Forks: "forks", ForkValues: "fork_values", ForkRefs: "fork_refs", Fields: "outs", Values: "outs_values", FieldRefs: "outs_refs",
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// resolve resolves the references the task in cwd posted, as the driver
// does: each item it staged, in/u<i>, is items' i-th, and each refs/<i>
// the bundle of its i-th file staged by reference.
func (r *refRun) resolve(work string, items, refs []string) {
	r.t.Helper()

	raw, err := os.ReadFile("outs_refs")
	if err != nil {
		r.t.Fatal(err)
	}

	if strings.Contains(string(raw), "\n") {
		r.t.Fatalf("outs_refs %q spans lines, want one", raw)
	}

	for rec := range strings.SplitSeq(string(raw), " ") {
		if rec == "" {
			continue
		}

		f := strings.Split(rec, "\t")
		kind, i, _ := strings.Cut(f[1], "/")

		n, err := strconv.Atoi(strings.TrimPrefix(i, "u"))
		if err != nil {
			r.t.Fatalf("reference %q: %v", rec, err)
		}

		src := items[n-1]
		if kind == "refs" {
			src = filepath.Dir(filepath.Dir(refs[n-1]))
		}

		item := filepath.Join(work, f[0])
		r.itemRefs[item] = append(r.itemRefs[item], filepath.Join(src, f[2]))
	}
}

// pipeline runs refMRO and publishes its pipeline outs, and returns READ's
// _args, PASS's posted items, and the published tree and outs JSON.
func (r *refRun) pipeline() (string, []string, map[string]string) {
	r.t.Helper()

	made, _ := r.task("P.MAKE", nil, func(_ map[string]string, files string) map[string]any {
		return map[string]any{"made": writeFileOf(r.t, filepath.Join(files, "made.txt"), "made")}
	})

	passed, _ := r.task("P.PASS", made, func(args map[string]string, _ string) map[string]any {
		return map[string]any{"y": args["x"]}
	})

	_, read := r.task("P.READ", append(passed, made...), func(map[string]string, string) map[string]any { return nil })

	return read, passed, r.publish(append(passed, made...))
}

// publish runs the OUTS task on the items, which names each staged item
// and bundle staged by reference by its producer's work dir.
func (r *refRun) publish(items []string) map[string]string {
	r.t.Helper()

	task := filepath.Join(r.root, "outs-task")
	if err := os.MkdirAll(task, dirPerm); err != nil {
		r.t.Fatal(err)
	}

	r.t.Chdir(task)

	refs := r.stage(items)
	locations := map[string]string{}

	for i, it := range items {
		abs, err := filepath.Abs(filepath.Join("in", "u"+strconv.Itoa(i+1)))
		if err != nil {
			r.t.Fatal(err)
		}

		locations[abs] = it
	}

	for i, o := range refs {
		abs, err := filepath.Abs(filepath.Join("refs", strconv.Itoa(i+1)))
		if err != nil {
			r.t.Fatal(err)
		}

		locations[abs] = filepath.Dir(filepath.Dir(o))
	}

	spec := OutsSpec{In: []string{"in"}, Refs: "refs", Locations: locations, Work: "work", Manifest: "publish.json", OutsJSON: filepath.Join("results", "pipeline_outs.json")}
	if err := r.g.WriteOuts(r.t.Context(), spec); err != nil {
		r.t.Fatal(err)
	}

	publishManifest(r.t, "publish.json", "results", func(src string) string { return src })

	return treeOf(r.t, "results")
}

// treeOf maps each file below dir to its content.
func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()

	tree := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, err := os.ReadFile(path)
		tree[strings.TrimPrefix(path, dir+"/")] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return tree
}

// TestPostNamesPassedFileByReference (#562): PASS returns the file MAKE
// wrote, which its task staged a copy of, as on an object store, so its
// post names it by reference to MAKE's file and writes no copy of it, which
// an object store would upload again. READ, given PASS's output and MAKE's,
// stages MAKE's file once, with MAKE's output, and nothing by reference,
// and gets the _args, to the byte, that it gets from a copy: one file,
// linked at one path for both inputs. The pipeline outs, which read both
// outputs too, publish the same tree and outs JSON.
func TestPostNamesPassedFileByReference(t *testing.T) {
	copied := newRefRun(t, false)
	copyArgs, copyPassed, copyTree := copied.pipeline()

	referred := newRefRun(t, true)
	refArgs, refPassed, refTree := referred.pipeline()

	if want := `{"made":"<task>/args/f/L0000/made.txt","y":"<task>/args/f/L0000/made.txt"}`; copyArgs != want {
		t.Errorf("READ's _args from copies %s, want %s", copyArgs, want)
	}

	if refArgs != copyArgs {
		t.Errorf("READ's _args by reference %s, from copies %s", refArgs, copyArgs)
	}

	if d := cmp.Diff(copyTree, refTree); d != "" {
		t.Errorf("published by reference (-copies +references):\n%s", d)
	}

	if got := leafNames(t, copyPassed[0]); len(got) != 2 {
		t.Errorf("PASS posted %v from copies, want the file and its origin record", got)
	}

	if _, err := os.Stat(filepath.Join(refPassed[0], "f")); !os.IsNotExist(err) {
		t.Errorf("PASS posted a files dir by reference (%v), want none", err)
	}

	want := []string{filepath.Join(referred.root, "work", "P.MAKE", "outs", "made", "f", "L0000")}
	if d := cmp.Diff(want, referred.itemRefs[refPassed[0]]); d != "" {
		t.Errorf("PASS's y names by reference (-want +got):\n%s", d)
	}

	for _, task := range []string{filepath.Join("scratch", "P.READ"), "outs-task"} {
		if got, err := os.ReadDir(filepath.Join(referred.root, task, "refs")); err != nil || len(got) != 0 {
			t.Errorf("%s staged %v by reference (%v), want MAKE's file staged once, with MAKE's output", task, got, err)
		}
	}
}
