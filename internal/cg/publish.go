package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

var (
	errNoLocation  = errors.New("no location for a staged outs bundle")
	errUnpublished = errors.New("published file missing")
	errStrayPath   = errors.New("pipeline outs name a path of the reconstructed pipestance")
)

// pipestance is the pipestance Martian's post-processing publishes the
// pipeline outs from. Each file leaf is placed where mrp's pipestance would
// hold it: a stage's own file at one path in the pipestance however many
// outputs reach it, inside the directory of the stage's that holds it, which
// PostProcess moves into outs/ at the first and links the others to, and a
// Linked file at its own path outside the pipestance for each output that
// reaches it, which PostProcess links into outs/ there.
type pipestance struct {
	dir, outs, linked string
	tbl               *types.Table
	params            []ir.Param
	// locations maps each staged outs bundle to where its producing task's
	// work dir keeps it, and tasks the dir each such task ran in to its work
	// dir.
	locations, tasks map[string]string
	// origins maps the origin of each stage file the pipeline outs reach to
	// a leaf holding it, and placed each origin copied to the pipestance to
	// its path there.
	origins, placed map[string]string
	// names maps a placed path to the name of the file it holds when the
	// pipeline outs leave it unpublished.
	names map[string]string
	// links maps a Linked file's placed path to its link in outs/.
	links map[string]string
	// entry maps each entry file value the task staged to the path it reads
	// it at.
	entry map[string]string
	n     int
}

func newPipestance(spec OutsSpec, items []string, tbl *types.Table, params []ir.Param) (*pipestance, error) {
	work, err := filepath.Abs(spec.Work)
	if err != nil {
		return nil, fmt.Errorf("pipeline outs: %w", err)
	}

	// Martian tells a path outside the pipestance by a substring test
	// (post_process.go moveOutFile), which no sibling of it under work meets.
	p := &pipestance{
		dir: filepath.Join(work, "pipestance"), linked: filepath.Join(work, "linked"), tbl: tbl, params: params,
		locations: map[string]string{}, tasks: map[string]string{},
		origins: map[string]string{}, placed: map[string]string{}, names: map[string]string{}, links: map[string]string{},
		entry: spec.EntryFiles,
	}
	p.outs = filepath.Join(p.dir, "outs")

	for _, item := range items {
		if err := p.locate(spec.Locations, item); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// locate records where the producing task of the staged outs bundle item
// keeps it, and the work dir of the dir that task ran in.
func (p *pipestance) locate(locations map[string]string, item string) error {
	abs, err := filepath.Abs(item)
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	loc, ok := locations[abs]
	if !ok {
		return nil
	}

	p.locations[abs] = loc

	task, ok, err := readTask(item)
	if err != nil || !ok {
		return err
	}

	if w, ok := strings.CutSuffix(loc, "/"+task.At); ok {
		p.tasks[task.Dir] = w
	}

	return nil
}

// reconstruct writes the pipeline outs v to the top fork's _outs at path, as
// mrp's resolver writes them, each file leaf placed in the pipestance.
func (p *pipestance) reconstruct(v json.Marshaler, path string) error {
	raw, err := marshal(v)
	if err != nil {
		return err
	}

	payload, err := decodePayload(raw)
	if err != nil {
		return err
	}

	if payload, err = p.tbl.Apply(p.params, payload, p.staged); err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	if _, err := p.tbl.Apply(p.params, payload, p.survey); err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	placed, err := p.tbl.Apply(p.params, payload, p.place)
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	return writeJSONFile(path, placed)
}

// staged is the path the task reads the file leaf at: an entry file value
// where the task staged it, which Martian's post-processing then finds.
func (p *pipestance) staged(leaf string) (string, error) {
	if at, ok := p.entry[leaf]; ok {
		return at, nil
	}

	return leaf, nil
}

// survey records a leaf holding each stage file the pipeline outs reach, by
// its origin, so place can put a file inside a stage's directory inside that
// directory whichever output reaches it first.
func (p *pipestance) survey(leaf string) (string, error) {
	o, ok, err := p.origin(leaf)
	if err != nil || !ok || o.Linked {
		return leaf, err
	}

	if _, ok := p.origins[o.Path]; !ok {
		p.origins[o.Path] = leaf
	}

	return leaf, nil
}

// place links the file leaf at leaf into the pipestance and returns its path
// there. A path naming no file is a file the stage never wrote, which
// PostProcess reads as mrp does.
func (p *pipestance) place(leaf string) (string, error) {
	o, ok, err := p.origin(leaf)
	if err != nil || !ok {
		return leaf, err
	}

	var at string
	if o.Linked {
		at, err = p.copy(leaf, p.linked)
	} else {
		at, err = p.placeOwn(o.Path)
	}

	if err != nil {
		return "", err
	}

	if _, ok := p.names[at]; !ok {
		p.names[at] = o.name
	}

	return at, nil
}

// placeOwn returns the path in the pipestance of the stage's own file at
// origin: one path however many outputs reach it, inside the outermost
// directory of the stage's that holds it, as mrp's pipestance holds one tree.
func (p *pipestance) placeOwn(origin string) (string, error) {
	top := origin
	for dir := filepath.Dir(origin); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, ok := p.origins[dir]; ok {
			top = dir
		}
	}

	at, ok := p.placed[top]
	if !ok {
		var err error
		if at, err = p.copy(p.origins[top], filepath.Join(p.dir, "files")); err != nil {
			return "", err
		}

		p.placed[top] = at
	}

	return filepath.Join(at, strings.TrimPrefix(origin, top)), nil
}

// copy copies the file leaf at leaf to a dir of its own below root and
// returns the copy's path.
func (p *pipestance) copy(leaf, root string) (string, error) {
	p.n++
	at := filepath.Join(root, strconv.Itoa(p.n), filepath.Base(leaf))

	if err := os.MkdirAll(filepath.Dir(at), dirPerm); err != nil {
		return "", fmt.Errorf("pipeline outs: %w", err)
	}

	if err := shim.CopyTree(leaf, at); err != nil {
		return "", fmt.Errorf("pipeline outs: place %s: %w", leaf, err)
	}

	return at, nil
}

// leafOrigin is the origin of a file leaf and its name when left
// unpublished.
type leafOrigin struct {
	shim.Origin

	name string
}

// origin identifies the file leaf at leaf, and reports false for a path
// naming no file.
func (p *pipestance) origin(leaf string) (leafOrigin, bool, error) {
	if _, err := os.Lstat(leaf); errors.Is(err, fs.ErrNotExist) {
		return leafOrigin{}, false, nil
	} else if err != nil {
		return leafOrigin{}, false, fmt.Errorf("pipeline outs: %w", err)
	}

	o, err := p.identify(leaf)

	return o, err == nil, err
}

// identify returns the origin of the file leaf at leaf and its name when
// left unpublished: where its producer's work dir keeps the copy staged
// here, or for a file the invocation names, which no bundle holds, its own
// path.
func (p *pipestance) identify(leaf string) (leafOrigin, error) {
	item, rel, ok := shim.LeafBundle(leaf)
	if !ok {
		return leafOrigin{shim.Origin{Path: leaf, Linked: true}, leaf}, nil
	}

	o, recorded, err := shim.ReadOrigin(leaf)
	if err != nil {
		return leafOrigin{}, fmt.Errorf("pipeline outs: %w", err)
	}

	if !recorded {
		o = shim.Origin{Path: leaf}
	}

	loc, ok := p.locations[item]
	if !ok {
		return leafOrigin{}, fmt.Errorf("%w: %s", errNoLocation, item)
	}

	return leafOrigin{o, loc + "/" + filepath.ToSlash(rel)}, nil
}

// published reads the post-processed outs at path and names each file as
// the published outs JSON does: a published file by its path in the
// published tree, a Linked one by the path of its link there, and one left
// unpublished where its producer keeps it.
func (p *pipestance) published(path string) (map[string]any, error) {
	if err := p.indexLinks(); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("post-processed outs: %w", err)
	}

	payload, err := decodePayload(raw)
	if err != nil {
		return nil, err
	}

	outs, err := p.tbl.Apply(p.params, payload, p.name)
	if err != nil {
		return nil, fmt.Errorf("post-processed outs: %w", err)
	}

	return outs, nil
}

func (p *pipestance) name(path string) (string, error) {
	if rel, ok := strings.CutPrefix(path, p.outs+string(filepath.Separator)); ok {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("%w: %w", errUnpublished, err)
		}

		return filepath.ToSlash(rel), nil
	}

	if link, ok := p.links[path]; ok {
		return link, nil
	}

	if name, ok := p.names[path]; ok {
		return name, nil
	}

	if within(path, p.dir) || within(path, p.linked) {
		return "", fmt.Errorf("%w: %s", errStrayPath, path)
	}

	return p.declared(path), nil
}

// declared names a path a stage declared below the dir its task ran in, a
// file it never wrote, below that task's work dir, which a scratch dir is
// not.
func (p *pipestance) declared(path string) string {
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if w, ok := p.tasks[dir]; ok {
			return w + "/" + filepath.ToSlash(strings.TrimPrefix(path, dir+string(filepath.Separator)))
		}
	}

	return path
}

// indexLinks maps each Linked file to the link PostProcess made to it in
// outs/.
func (p *pipestance) indexLinks() error {
	if _, err := os.Lstat(p.outs); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	err := filepath.WalkDir(p.outs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}

		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read link: %w", err)
		}

		if within(target, p.linked) {
			p.links[target] = filepath.ToSlash(strings.TrimPrefix(path, p.outs+string(filepath.Separator)))
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("published links: %w", err)
	}

	return nil
}

// materialize writes the published tree at outs/ into results, each link
// replaced by what it reaches, since a publish dir may be an object store.
func (p *pipestance) materialize(results string) error {
	if err := os.MkdirAll(results, dirPerm); err != nil {
		return fmt.Errorf("published tree: %w", err)
	}

	if _, err := os.Lstat(p.outs); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	err := filepath.WalkDir(p.outs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == p.outs {
			return err
		}

		rel := strings.TrimPrefix(path, p.outs+string(filepath.Separator))
		dst := filepath.Join(results, rel)

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if _, err := os.Stat(path); err != nil {
				return danglingLink(filepath.ToSlash(rel), err)
			}

			return shim.CopyTree(path, dst)
		case d.IsDir():
			return os.MkdirAll(dst, dirPerm)
		default:
			return shim.CopyTree(path, dst)
		}
	})
	if err != nil {
		return fmt.Errorf("published tree: %w", err)
	}

	return nil
}

// danglingLink is the error for the link at rel in outs/ that reaches nothing.
// A link below a published directory is the one PostProcess leaves where it
// moved out a file that the pipeline outs publish separately from the
// directory. The copy of a stage's directory drops its own dangling links, so
// no other link can lie there.
func danglingLink(rel string, err error) error {
	dir, _, nested := strings.Cut(rel, "/")
	if !nested {
		return fmt.Errorf("%w: %w", errUnpublished, err)
	}

	return fmt.Errorf("%w: a file inside the published directory output %s is also published separately, "+
		"so outs/%s is a link that reaches nothing; mrp leaves this dangling link and succeeds, "+
		"but a results dir, possibly an object store, cannot hold it (publish the directory or the file inside it, not both): %w",
		errUnpublished, dir, rel, err)
}

// within reports whether path lies below dir.
func within(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// decodePayload decodes an outs object, its values in their encoded key
// order and number lexemes.
func decodePayload(raw []byte) (map[string]any, error) {
	v, err := types.DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode outs: %w", err)
	}

	payload := map[string]any{}

	if obj, ok := types.AsObject(v); ok {
		for _, m := range obj {
			payload[m.Key] = m.Value
		}
	}

	return payload, nil
}

// writeJSONFile writes v as mrp writes an outs JSON.
func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, raw, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
