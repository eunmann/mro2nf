package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoLocation  = errors.New("no location for a staged outs bundle")
	errUnpublished = errors.New("published file missing")
	errStrayPath   = errors.New("pipeline outs name a path of the reconstructed pipestance")
	errNoIdentity  = errors.New("no device and inode for a placed path")
	errNoSource    = errors.New("published path placed from no staged file")
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
	shape             forkShape
	// locations maps each staged outs bundle, and each bundle holding a
	// leaf staged by reference, to where its producing task's work dir
	// keeps it, and tasks the dir each task posting an outs bundle ran in
	// to its work dir.
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
	// sources maps each placed path, by its file's identity, which
	// PostProcess's rename keeps, to the file the run publishes it from.
	sources map[fileKey]placedSource
	n       int
}

// placedSource is the file the run publishes a placed path from: a file a
// stage task wrote, by where its task keeps it (task), or any other, an
// entry file or a host path, by its path here (external), which no task
// dir holds and so Nextflow publishes only once a task writes it.
type placedSource struct {
	task, external string
}

// below is the source of the path rel below a placed dir that s is the
// source of.
func (s placedSource) below(rel string) placedSource {
	if s.external != "" {
		return placedSource{external: s.external + rel}
	}

	return placedSource{task: s.task + filepath.ToSlash(rel)}
}

// fileKey identifies a file by its device and inode, as shim's dirID: int64
// keeps the conversions from Stat_t's per-platform field types necessary.
type fileKey struct {
	dev, ino int64
}

// forkShape is the shape of the top-level call's outs: params are its
// pipeline's outs, tbl the walk over their types, and typ the type of the
// call's outs, an array or a map of the pipeline's, one per fork, for a
// top-level map call.
type forkShape struct {
	tbl    *types.Table
	params []*syntax.StructMember
	typ    syntax.Type
}

// each maps the file leaves of each fork's outs in v, the top-level call's
// outs, by fn, as Martian's PostProcess reads the forks' outs
// (core/post_process.go postProcess): each element of a map call's array
// or map, else v.
func (s forkShape) each(v any, fn types.Transform) (any, error) {
	apply := func(outs any) (any, error) {
		mapped, err := s.tbl.Apply(s.params, members(outs), fn)
		if err != nil {
			return nil, fmt.Errorf("file leaves: %w", err)
		}

		return mapped, nil
	}

	switch s.typ.(type) {
	case *syntax.ArrayType:
		forks, _ := v.([]any)
		out := make([]any, len(forks))

		for i, f := range forks {
			var err error
			if out[i], err = apply(f); err != nil {
				return nil, fmt.Errorf("fork %d: %w", i, err)
			}
		}

		return out, nil
	case *syntax.TypedMapType:
		forks, _ := types.AsObject(v)
		out := make(map[string]any, len(forks))

		for _, f := range forks {
			var err error
			if out[f.Key], err = apply(f.Value); err != nil {
				return nil, fmt.Errorf("fork %q: %w", f.Key, err)
			}
		}

		return out, nil
	default:
		return apply(v)
	}
}

func newPipestance(spec OutsSpec, items []string, shape forkShape) (*pipestance, error) {
	work, err := filepath.Abs(spec.Work)
	if err != nil {
		return nil, fmt.Errorf("pipeline outs: %w", err)
	}

	// Martian tells a path outside the pipestance by a substring test
	// (post_process.go moveOutFile), which no sibling of it under work meets.
	p := &pipestance{
		dir: filepath.Join(work, "pipestance"), linked: filepath.Join(work, "linked"), shape: shape,
		locations: spec.Locations, tasks: map[string]string{},
		origins: map[string]string{}, placed: map[string]string{}, names: map[string]string{}, links: map[string]string{},
		entry: spec.EntryFiles, sources: map[fileKey]placedSource{},
	}
	p.outs = filepath.Join(p.dir, "outs")

	for _, item := range items {
		if err := p.locate(item); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// locate records the work dir of the dir the producing task of the staged
// outs bundle item ran in.
func (p *pipestance) locate(item string) error {
	abs, err := filepath.Abs(item)
	if err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	loc, ok := p.locations[abs]
	if !ok {
		return nil
	}

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

	payload, err := types.DecodeJSON(raw)
	if err != nil {
		return fmt.Errorf("decode outs: %w", err)
	}

	if payload, err = p.shape.each(payload, p.staged); err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	if _, err := p.shape.each(payload, p.survey); err != nil {
		return fmt.Errorf("pipeline outs: %w", err)
	}

	placed, err := p.shape.each(payload, p.place)
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
		at, err = p.copy(leaf, p.linked, linkedSource(leaf, o))
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
		leaf := p.origins[top]

		o, err := p.identify(leaf)
		if err != nil {
			return "", err
		}

		if at, err = p.copy(leaf, filepath.Join(p.dir, "files"), placedSource{task: o.name}); err != nil {
			return "", err
		}

		p.placed[top] = at
	}

	return filepath.Join(at, strings.TrimPrefix(origin, top)), nil
}

// linkedSource is the source of the Linked file leaf at leaf, of origin o:
// a bundle's leaf where its task keeps it, and any other file, which no
// stage task wrote, itself.
func linkedSource(leaf string, o leafOrigin) placedSource {
	if _, _, ok := shim.LeafBundle(leaf); ok {
		return placedSource{task: o.name}
	}

	return placedSource{external: leaf}
}

// copy places the shape of the file leaf at leaf, whose source is src, in
// a dir of its own below root, and returns its path there. Post-processing
// reads no file's bytes, so none is copied.
func (p *pipestance) copy(leaf, root string, src placedSource) (string, error) {
	p.n++
	at := filepath.Join(root, strconv.Itoa(p.n), filepath.Base(leaf))

	if err := os.MkdirAll(filepath.Dir(at), dirPerm); err != nil {
		return "", fmt.Errorf("pipeline outs: %w", err)
	}

	if err := shim.CopyShape(leaf, at); err != nil {
		return "", fmt.Errorf("pipeline outs: place %s: %w", leaf, err)
	}

	return at, p.recordSources(at, src)
}

// recordSources records the source of each path of the tree placed at at:
// src, and below it, the same path below src.
func (p *pipestance) recordSources(at string, src placedSource) error {
	err := filepath.WalkDir(at, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		k, err := keyOf(path)
		if err != nil {
			return err
		}

		p.sources[k] = src.below(strings.TrimPrefix(path, at))

		return nil
	})
	if err != nil {
		return fmt.Errorf("pipeline outs: place %s: %w", at, err)
	}

	return nil
}

// keyOf is the identity of the file at path, its links followed.
func keyOf(path string) (fileKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileKey{}, fmt.Errorf("stat %s: %w", path, err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, fmt.Errorf("%w: %s", errNoIdentity, path)
	}

	return fileKey{dev: int64(st.Dev), ino: int64(st.Ino)}, nil
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
func (p *pipestance) published(path string) (any, error) {
	if err := p.indexLinks(); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("post-processed outs: %w", err)
	}

	payload, err := types.DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode outs: %w", err)
	}

	outs, err := p.shape.each(payload, p.name)
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

// externalDir is the dir beside the manifest that OUTS copies each
// published file no stage task wrote into, from which the run publishes it.
const externalDir = "external"

// published is one entry of the published tree: its path in the tree and
// the file the run publishes it from, where the task that wrote it keeps
// it, or, Local, a path beside the manifest.
type published struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	Local  bool   `json:"local,omitempty"`
}

// manifest writes to path the published tree at outs/ as the entries the
// run publishes, by path: each file PostProcess moved or linked into
// outs/, a link as what it reaches, since a publish dir may be an object
// store, and a placed dir whole.
func (p *pipestance) manifest(path string) error {
	entries, err := p.entries(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("published tree: %w", err)
	}

	slices.SortFunc(entries, func(a, b published) int { return strings.Compare(a.Path, b.Path) })

	return writeRecord(path, entries)
}

// entries are the entries of the published tree at outs/, none where
// PostProcess published nothing, each external one copied below dir.
func (p *pipestance) entries(dir string) ([]published, error) {
	entries := []published{}

	if _, err := os.Lstat(p.outs); errors.Is(err, fs.ErrNotExist) {
		return entries, nil
	}

	err := filepath.WalkDir(p.outs, func(at string, d fs.DirEntry, err error) error {
		if err != nil || at == p.outs {
			return err
		}

		rel := filepath.ToSlash(strings.TrimPrefix(at, p.outs+string(filepath.Separator)))

		src, whole, err := p.source(at, rel, d)
		if err != nil || src == (placedSource{}) {
			return err
		}

		e, err := p.publishedAt(rel, src, dir)
		if err != nil {
			return err
		}

		entries = append(entries, e)

		if whole {
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk outs/: %w", err)
	}

	return entries, nil
}

// publishedAt is the entry at rel in the tree published from src: a file
// a stage task wrote from where its task keeps it, and any other from its
// copy below dir, external/<n>/, as Nextflow publishes only a file a task
// wrote.
func (p *pipestance) publishedAt(rel string, src placedSource, dir string) (published, error) {
	if src.external == "" {
		return published{Path: rel, Source: src.task}, nil
	}

	p.n++
	local := path.Join(externalDir, strconv.Itoa(p.n), path.Base(rel))
	at := filepath.Join(dir, filepath.FromSlash(local))

	if err := os.MkdirAll(filepath.Dir(at), dirPerm); err != nil {
		return published{}, fmt.Errorf("publish outs/%s: %w", rel, err)
	}

	if err := shim.CopyTree(src.external, at); err != nil {
		return published{}, fmt.Errorf("publish outs/%s: %w", rel, err)
	}

	return published{Path: rel, Source: local, Local: true}, nil
}

// source is the source of the entry d at at, rel in outs/, and whether it
// is a placed dir, published whole; none for a dir PostProcess made, whose
// entries are published each.
func (p *pipestance) source(at, rel string, d fs.DirEntry) (placedSource, bool, error) {
	if d.Type()&fs.ModeSymlink != 0 {
		if _, err := os.Stat(at); err != nil {
			return placedSource{}, false, danglingLink(rel, err)
		}
	}

	k, err := keyOf(at)
	if err != nil {
		return placedSource{}, false, err
	}

	src, ok := p.sources[k]

	switch {
	case !ok && d.IsDir():
		return placedSource{}, false, nil
	case !ok:
		return placedSource{}, false, fmt.Errorf("%w: outs/%s", errNoSource, rel)
	case d.IsDir():
		return src, true, reachesAll(at, rel)
	default:
		return src, false, nil
	}
}

// reachesAll fails on a link below the placed dir at at, rel in outs/,
// that reaches nothing: the link PostProcess leaves where it moved out an
// entry the pipeline outs publish apart from the dir.
func reachesAll(at, rel string) error {
	err := filepath.WalkDir(at, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}

		if _, err := os.Stat(path); err != nil {
			return danglingLink(rel+filepath.ToSlash(strings.TrimPrefix(path, at)), err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("published dir outs/%s: %w", rel, err)
	}

	return nil
}

// danglingLink is the error for the link at rel in outs/ that reaches nothing.
// A link below a published directory is the one PostProcess leaves where it
// moved out an entry that the pipeline outs publish separately from the
// directory. The copy of a stage's directory drops its own dangling links, so
// no other link can lie there. The message names only the link: the
// directory may sit inside a map, struct or array output at any depth.
func danglingLink(rel string, err error) error {
	if !strings.Contains(rel, "/") {
		return fmt.Errorf("%w: %w", errUnpublished, err)
	}

	return fmt.Errorf("%w: outs/%s is a link that reaches nothing, left where an entry of a published directory "+
		"was moved out because the pipeline outs also publish it separately; mrp leaves this dangling link and succeeds, "+
		"but a results dir, possibly an object store, cannot hold it (publish the directory or the entry inside it, not both): %w",
		errUnpublished, rel, err)
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

	return members(v), nil
}

// members is the decoded JSON object v as a map of its members, which is
// empty for any other value.
func members(v any) map[string]any {
	out := map[string]any{}

	if obj, ok := types.AsObject(v); ok {
		for _, m := range obj {
			out[m.Key] = m.Value
		}
	}

	return out
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
