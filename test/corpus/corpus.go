// Package corpus loads real-world Martian pipelines as invocations the
// call-graph sweeps and the Nextflow lint run beside the fixtures: the MIT
// sources vendored under testdata/corpus, and the MROPATH that Env names.
// A .mro file under a source's roots that holds a call is its own
// invocation. A file without one yields an invocation of each pipeline and
// stage it declares, with every input unset (Invocation). Each entry
// carries Martian's verdict, so what mrp would refuse is recorded, never
// run.
package corpus

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/fileio"
	"github.com/martian-lang/martian/martian/syntax"
)

// Env names an external corpus as an MROPATH: its roots are the include
// path of every invocation, and every .mro file below them is loaded.
const Env = "MRO2NF_CORPUS"

// ExternalSource is the name of the source Env names.
const ExternalSource = "external"

// Source is a tree of .mro files and the include path they compile with.
type Source struct {
	Name  string
	Roots []string
}

// Entry is one invocation of a corpus pipeline or stage.
type Entry struct {
	// Name is the source, the declaring file below the source's roots, and,
	// for a synthesized invocation, the callable: rnascp/pipeline.mro/GHOST.
	Name    string
	Mro     string
	MroPath []string
	// Rejected is Martian's refusal of the invocation, as mrp would refuse
	// it before any stage runs; nil when Martian accepts it.
	Rejected error
}

var (
	errSlugCollision = errors.New("corpus entries share a slug")
	errMartianPanic  = errors.New("martian panicked")
	errNoRoots       = errors.New("names no root")
)

// Sources are the vendored sources and the external source Env names, if
// set.
func Sources(vendored string) ([]Source, error) {
	out, err := Vendored(vendored)
	if err != nil || os.Getenv(Env) == "" {
		return out, err
	}

	ext, err := External(os.Getenv(Env))
	if err != nil {
		return nil, err
	}

	return append(out, ext), nil
}

// Vendored are the sources below vendored, one per directory, each its own
// include root.
func Vendored(vendored string) ([]Source, error) {
	dirs, err := os.ReadDir(vendored)
	if err != nil {
		return nil, fmt.Errorf("vendored corpus: %w", err)
	}

	var out []Source

	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}

		root, err := filepath.Abs(filepath.Join(vendored, d.Name()))
		if err != nil {
			return nil, fmt.Errorf("vendored corpus: %w", err)
		}

		out = append(out, Source{Name: d.Name(), Roots: []string{root}})
	}

	return out, nil
}

// External is the source whose roots the MROPATH mropath lists.
func External(mropath string) (Source, error) {
	src := Source{Name: ExternalSource}

	for _, r := range filepath.SplitList(mropath) {
		if r == "" {
			continue
		}

		root, err := filepath.Abs(r)
		if err == nil {
			root, err = filepath.EvalSymlinks(root)
		}

		if err != nil {
			return Source{}, fmt.Errorf("%s root %s: %w", Env, r, err)
		}

		src.Roots = append(src.Roots, root)
	}

	if len(src.Roots) == 0 {
		return Source{}, fmt.Errorf("%s=%q: %w", Env, mropath, errNoRoots)
	}

	return src, nil
}

// Load writes the invocations of every source's files below dir and
// returns the entries in source, file and declaration order.
func Load(dir string, sources []Source) ([]Entry, error) {
	var out []Entry

	slugs := map[string]string{}

	for _, src := range sources {
		files, err := Files(src.Roots)
		if err != nil {
			return nil, err
		}

		base := commonDir(src.Roots)

		for _, f := range files {
			rel, err := filepath.Rel(base, f)
			if err != nil {
				return nil, fmt.Errorf("corpus %s: %w", src.Name, err)
			}

			entries, err := fileEntries(dir, src.Roots, src.Name+"/"+filepath.ToSlash(rel), f)
			if err != nil {
				return nil, err
			}

			for _, e := range entries {
				if prev, ok := slugs[Slug(e.Name)]; ok {
					return nil, fmt.Errorf("%w: %s and %s", errSlugCollision, prev, e.Name)
				}

				slugs[Slug(e.Name)] = e.Name
			}

			out = append(out, entries...)
		}
	}

	return out, nil
}

// Accepted are the entries Martian accepts.
func Accepted(entries []Entry) []Entry {
	return slices.DeleteFunc(slices.Clone(entries), func(e Entry) bool { return e.Rejected != nil })
}

// Slug is name with every rune but an ASCII letter, digit or underscore
// replaced by an underscore: a directory name, and a Nextflow project's,
// which the lint gate keeps free of '-'.
func Slug(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}

		return '_'
	}, name)
}

// Files are the .mro files below roots, each once, sorted.
func Files(roots []string) ([]string, error) {
	seen := map[string]bool{}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if !d.IsDir() && filepath.Ext(path) == ".mro" {
				seen[path] = true
			}

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("corpus root %s: %w", root, err)
		}
	}

	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}

	slices.Sort(files)

	return files, nil
}

// commonDir is the deepest directory every root is below or at.
func commonDir(roots []string) string {
	base := roots[0]

	for _, r := range roots[1:] {
		for base != filepath.Dir(base) && r != base && !strings.HasPrefix(r, base+string(filepath.Separator)) {
			base = filepath.Dir(base)
		}
	}

	return base
}

// fileEntries are the entries of the .mro file at path, named name: the
// file when Martian refuses it or it holds a call, else an invocation below
// dir of each pipeline and stage it declares.
func fileEntries(dir string, roots []string, name, path string) ([]Entry, error) {
	_, _, ast, err := syntax.Compile(path, roots, false)
	if err != nil {
		return []Entry{{Name: name, Mro: path, MroPath: roots, Rejected: fmt.Errorf("compile: %w", err)}}, nil
	}

	if ast.Call != nil {
		return []Entry{{Name: name, Mro: path, MroPath: roots, Rejected: Verdict(path, roots)}}, nil
	}

	var out []Entry

	for _, c := range ast.Callables.List {
		if c.File() == nil || c.File().FullPath != path {
			continue
		}

		e := Entry{Name: name + "/" + c.GetId(), MroPath: roots}
		e.Mro = filepath.Join(dir, Slug(e.Name), "pipeline.mro")

		if err := fileio.Write(e.Mro, []byte(Invocation(path, c))); err != nil {
			return nil, fmt.Errorf("corpus invocation: %w", err)
		}

		e.Rejected = Verdict(e.Mro, roots)
		out = append(out, e)
	}

	return out, nil
}

// Invocation calls c, declared in the file at path, with each input
// unset: null, but a bool false, as Martian refuses a null bound to a
// disable (syntax/resolve_stage.go: "disabled cannot be bound to a null
// value").
func Invocation(path string, c syntax.Callable) string {
	var b strings.Builder

	fmt.Fprintf(&b, "@include %q\n\ncall %s(\n", path, c.GetId())

	for _, in := range c.GetInParams().List {
		v := "null"
		if in.Tname.Tname == syntax.KindBool && in.Tname.ArrayDim == 0 && in.Tname.MapDim == 0 {
			v = "false"
		}

		fmt.Fprintf(&b, "    %s = %s,\n", in.Id, v)
	}

	b.WriteString(")\n")

	return b.String()
}

// Verdict is Martian's refusal of the invocation at path, nil when it
// accepts it: mrp parses the invocation and builds its call graph before
// any stage runs (core/runtime.go instantiatePipeline).
func Verdict(path string, roots []string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read invocation: %w", err)
	}

	_, _, ast, err := syntax.ParseSourceBytes(src, path, roots, false)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}

	var p any

	func() {
		defer func() { p = recover() }()

		_, err = ast.MakePipelineCallGraph("", ast.Call)
	}()

	if p != nil {
		return fmt.Errorf("call graph: %w: %v", errMartianPanic, p)
	}

	if err != nil {
		return fmt.Errorf("call graph: %w", err)
	}

	return nil
}
