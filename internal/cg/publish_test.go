package cg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// publishMRO is a pipeline whose outs reach a stage's file directly and
// through a stage that returns the file it was given, a stage's link to a
// host file from two outputs, and files below a 2-D array.
const publishMRO = `filetype txt;

stage MAKE(
    out txt     made,
    out txt[][] grid,
    src py      "make",
)

stage PASS(
    in  txt x,
    out txt y,
    src py  "pass",
)

stage LINK(
    out txt l,
    src py  "link",
)

pipeline P(
    out txt     made,
    out txt     passed,
    out txt     linked,
    out txt     relinked,
    out txt[][] grid,
)
{
    call MAKE()

    call PASS(
        x = MAKE.made,
    )

    call LINK()

    return (
        made     = MAKE.made,
        passed   = PASS.y,
        linked   = LINK.l,
        relinked = LINK.l,
        grid     = MAKE.grid,
    )
}

call P()
`

// publishRun is a run of publishMRO whose tasks each ran in a scratch dir
// and left their posted outs in their work dir, as on an object store.
type publishRun struct {
	t    *testing.T
	g    *Graph
	man  types.Manifest
	mro  string
	root string
	// makeWork is MAKE's work dir, and staged the outs bundles OUTS staged,
	// each in its producer's work dir by the path it is staged at.
	makeWork string
	staged   map[string]string
}

// nestedMRO is a pipeline whose outs reach a stage's directory and, through
// a stage that returns a path inside the directory it was given, a file
// inside it.
const nestedMRO = `filetype txt;

stage MKDIR(
    out path d,
    src py   "mkdir",
)

stage PICK(
    in  path d,
    out txt  x,
    src py   "pick",
)

pipeline P(
    out path d,
    out txt  x,
)
{
    call MKDIR()

    call PICK(
        d = MKDIR.d,
    )

    return (
        d = MKDIR.d,
        x = PICK.x,
    )
}

call P()
`

// newRun loads mro as a run's invocation.
func newRun(t *testing.T, mro string) *publishRun {
	t.Helper()

	root := t.TempDir()
	r := &publishRun{t: t, mro: filepath.Join(root, "pipeline.mro"), root: root, staged: map[string]string{}}
	writeFileOf(t, r.mro, mro)

	var err error
	if r.g, err = Load(r.mro, "", nil); err != nil {
		t.Fatal(err)
	}

	r.man = r.g.Manifest()

	return r
}

// newPublishRun runs MAKE, PASS and LINK, and stages the outs bundles the
// pipeline outs read into the OUTS task's dir, its working dir.
func newPublishRun(t *testing.T) *publishRun {
	t.Helper()

	r := newRun(t, publishMRO)
	host := writeFileOf(t, filepath.Join(r.root, "host", "ref.txt"), "host")

	r.makeWork = r.task("P.MAKE", nil, func(_ map[string]any, files string) map[string]any {
		cells := []any{writeFileOf(t, filepath.Join(files, "cell.txt"), "cell"), filepath.Join(files, "gone.txt")}

		return map[string]any{"made": writeFileOf(t, filepath.Join(files, "made.txt"), "made"), "grid": []any{cells}}
	})

	passWork := r.task("P.PASS", []string{filepath.Join(r.makeWork, "outs", "made")}, func(args map[string]any, _ string) map[string]any {
		return map[string]any{"y": args["x"]}
	})

	linkWork := r.task("P.LINK", nil, func(_ map[string]any, files string) map[string]any {
		if err := os.Symlink(host, filepath.Join(files, "l.txt")); err != nil {
			t.Fatal(err)
		}

		return map[string]any{"l": filepath.Join(files, "l.txt")}
	})

	r.stageOuts(filepath.Join(r.makeWork, "outs", "made"), filepath.Join(r.makeWork, "outs", "grid"), filepath.Join(passWork, "outs", "y"), filepath.Join(linkWork, "outs", "l"))

	return r
}

// stageOuts stages the outs bundles items into the OUTS task's dir, and
// makes it the working dir.
func (r *publishRun) stageOuts(items ...string) {
	r.t.Helper()

	task := filepath.Join(r.root, "outs-task")
	if err := os.MkdirAll(filepath.Join(task, "in"), 0o755); err != nil {
		r.t.Fatal(err)
	}

	r.t.Chdir(task)

	for i, item := range items {
		at := filepath.Join(task, "in", "u"+strconv.Itoa(i+1))
		if err := os.Symlink(item, at); err != nil {
			r.t.Fatal(err)
		}

		r.staged[at] = item
	}
}

// task runs node's task in its scratch dir: its args from the staged items
// in, then the outs its stage returns, written under the stage's
// confinement and posted output by output. It returns the work dir the
// posted outs are unstaged to.
func (r *publishRun) task(node string, in []string, outs func(args map[string]any, files string) map[string]any) string {
	r.t.Helper()

	scratch, work := filepath.Join(r.root, "scratch", node), filepath.Join(r.root, "work", node)
	files := filepath.Join(scratch, "main", "files")

	if err := os.MkdirAll(files, 0o755); err != nil {
		r.t.Fatal(err)
	}

	r.t.Chdir(scratch)

	if _, err := r.g.WriteArgs(ArgsSpec{Node: node, In: in, Manifest: r.man, Out: "args"}); err != nil {
		r.t.Fatal(err)
	}

	var args map[string]any

	raw, err := shim.ReadBundle("args")
	if err == nil {
		err = json.Unmarshal(raw, &args)
	}

	if err != nil {
		r.t.Fatal(err)
	}

	if err := r.post(node, scratch, outs(args, files)); err != nil {
		r.t.Fatal(err)
	}

	if err := shim.CopyTree(filepath.Join(scratch, "outs"), filepath.Join(work, "outs")); err != nil {
		r.t.Fatal(err)
	}

	return work
}

func (r *publishRun) post(node, scratch string, outs map[string]any) error {
	st, err := r.g.Stage(node)
	if err != nil {
		return err
	}

	params, err := r.man.Params(st.Call().DecId, types.RoleOut)
	if err != nil {
		return err
	}

	conf, err := shim.ConfineStage(scratch, "main", false)
	if err != nil {
		return err
	}

	if err := shim.WriteBundle("stage_outs", outs, params, r.man.Table(), conf); err != nil {
		return err
	}

	return r.g.Post(PostSpec{Args: "args", Outs: "stage_outs", Manifest: r.man, Forks: "forks", Fields: "outs"})
}

// writeOuts publishes the pipeline outs from the staged bundles with
// locations, into the working dir.
func (r *publishRun) writeOuts(locations map[string]string) error {
	spec := OutsSpec{In: []string{"in"}, Locations: locations, Manifest: r.man, Work: "work", Results: "results", OutsJSON: "pipeline_outs.json"}

	return r.g.WriteOuts(r.t.Context(), spec)
}

// TestWriteOutsPublishesAsMrp: the pipeline outs publish as mrp's
// post-processing publishes them. The file PASS returns is the MAKE file it
// was given, published once and named by its first path (#446); LINK's link
// to a host file is linked, so copied, at each output that reaches it
// (#485); and the files below MAKE's 2-D array stay unpublished, each named
// where MAKE's work dir keeps it, as is the file MAKE declared and never
// wrote, though MAKE ran in a scratch dir (#457).
func TestWriteOutsPublishesAsMrp(t *testing.T) {
	r := newPublishRun(t)

	locations := map[string]string{}
	for at, item := range r.staged {
		locations[at] = "s3://bucket" + item
	}

	if err := r.writeOuts(locations); err != nil {
		t.Fatal(err)
	}

	var got map[string]any

	raw, err := os.ReadFile("pipeline_outs.json")
	if err == nil {
		err = json.Unmarshal(raw, &got)
	}

	if err != nil {
		t.Fatal(err)
	}

	work := "s3://bucket" + r.makeWork
	want := map[string]any{
		"made": "made.txt", "passed": "made.txt", "linked": "linked.txt", "relinked": "relinked.txt",
		"grid": []any{[]any{work + "/outs/grid/f/L0000/cell.txt", work + "/main/files/gone.txt"}},
	}

	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("pipeline_outs.json (-mrp +got):\n%s", d)
	}

	tree := map[string]string{"made.txt": "made", "passed.txt": "made", "linked.txt": "host", "relinked.txt": "host"}
	if d := cmp.Diff(tree, publishedFiles(t, "results")); d != "" {
		t.Errorf("published tree (-mrp +got):\n%s", d)
	}
}

// TestWriteOutsHoldsOneTree: a file inside a stage's directory that the
// pipeline outs reach lies inside that directory in the pipestance, as in
// mrp's, so PostProcess moves it out of the directory it moved into outs/
// and leaves there a link that reaches nothing, which fails the pipeline outs
// rather than publish the file twice.
func TestWriteOutsHoldsOneTree(t *testing.T) {
	r := newRun(t, nestedMRO)

	mkdirWork := r.task("P.MKDIR", nil, func(_ map[string]any, files string) map[string]any {
		d := filepath.Join(files, "d")
		writeFileOf(t, filepath.Join(d, "sub", "x.txt"), "x")
		writeFileOf(t, filepath.Join(d, "y.txt"), "y")

		return map[string]any{"d": d}
	})

	pickWork := r.task("P.PICK", []string{filepath.Join(mkdirWork, "outs", "d")}, func(args map[string]any, _ string) map[string]any {
		d, _ := args["d"].(string)

		return map[string]any{"x": filepath.Join(d, "sub", "x.txt")}
	})

	r.stageOuts(filepath.Join(mkdirWork, "outs", "d"), filepath.Join(pickWork, "outs", "x"))

	locations := map[string]string{}
	for at, item := range r.staged {
		locations[at] = "s3://bucket" + item
	}

	err := r.writeOuts(locations)
	if !errors.Is(err, errUnpublished) {
		t.Fatalf("pipeline outs reaching a directory and a file inside it: %v, want %v", err, errUnpublished)
	}

	for _, want := range []string{
		"outs/d/sub/x.txt is a link that reaches nothing",
		"the pipeline outs also publish it separately",
		"mrp leaves this dangling link and succeeds",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	outs := filepath.Join("work", "pipestance", "outs")
	if info, err := os.Lstat(filepath.Join(outs, "d", "sub", "x.txt")); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("outs/d/sub/x.txt = %v, %v, want the link PostProcess leaves where it moved the file from", info, err)
	}

	if raw, err := os.ReadFile(filepath.Join(outs, "x.txt")); err != nil || string(raw) != "x" {
		t.Errorf("outs/x.txt = %q, %v, want the file inside the directory", raw, err)
	}
}

// TestWriteOutsNeedsLocations: a staged outs bundle without a location
// fails the pipeline outs rather than name its unpublished files nowhere.
func TestWriteOutsNeedsLocations(t *testing.T) {
	r := newPublishRun(t)

	if err := r.writeOuts(nil); !errors.Is(err, errNoLocation) {
		t.Errorf("pipeline outs without their bundles' locations: %v, want %v", err, errNoLocation)
	}
}

// TestPublishedNamesFailLoudly: a post-processed outs path the published outs
// cannot name as mrp does fails the pipeline outs rather than reach
// pipeline_outs.json: a reconstructed pipestance path no file was placed at,
// and a name in outs/ or a link there that reaches no file.
func TestPublishedNamesFailLoudly(t *testing.T) {
	p, err := newPipestance(OutsSpec{Work: t.TempDir()}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(p.outs, 0o755); err != nil {
		t.Fatal(err)
	}

	dangling := filepath.Join(p.outs, "dangling.txt")
	if err := os.Symlink(filepath.Join(p.linked, "1", "gone.txt"), dangling); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		path string
		want error
	}{
		"an unplaced pipestance path": {filepath.Join(p.dir, "files", "3", "x.txt"), errStrayPath},
		"an unplaced linked path":     {filepath.Join(p.linked, "3", "x.txt"), errStrayPath},
		"a missing name in outs":      {filepath.Join(p.outs, "missing.txt"), errUnpublished},
		"a dangling link in outs":     {dangling, errUnpublished},
	}

	for name, tc := range cases {
		if got, err := p.name(tc.path); !errors.Is(err, tc.want) {
			t.Errorf("name of %s = %q, %v, want %v", name, got, err, tc.want)
		}
	}

	err = p.materialize(filepath.Join(t.TempDir(), "results"))
	if !errors.Is(err, errUnpublished) {
		t.Errorf("materialize outs/ holding a dangling link: %v, want %v", err, errUnpublished)
	}

	if err != nil && strings.Contains(err.Error(), "published directory") {
		t.Errorf("a dangling link outside any directory: %v, want no directory shape named", err)
	}
}

// publishedFiles maps each file below dir to its content, failing on any
// entry that is not a regular file or a dir.
func publishedFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		if !d.Type().IsRegular() {
			t.Errorf("%s is published as %v, not a regular file", path, d.Type())
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		out[rel] = string(raw)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// writeFileOf writes content to a file at path, and returns path.
func writeFileOf(t *testing.T, path, content string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}
