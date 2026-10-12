package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eunmann/mro2nf/internal/frontend"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
)

var errNoCallable = errors.New("not a declared pipeline or stage")

// witnessWidth is the length of each entry collection in the witness; its
// nested collections differ in length element to element.
const witnessWidth = 2

// Source is an entry .mro split as a run rebuilds its invocation: the entry
// file's declarations and includes without its call, the call, and the
// invocation data of the call (core.InvocationData, mrp's JSON form of an
// invocation, as `mrg --reverse` writes it). Name is the entry file's name,
// which the rebuilt invocation keeps so _jobinfo records the include mrp
// records.
type Source struct {
	Name       string               `json:"name"`
	Decls      string               `json:"decls"`
	Call       string               `json:"call"`
	Invocation *core.InvocationData `json:"invocation"`
	// dir is the entry file's dir for a source ReadSource read. Martian
	// resolves a file's includes beside it first, so the declarations must
	// compile there and not in the working directory. A decoded source has
	// none: a run links it under Name in its task's dir.
	dir string
}

// ReadSource reads the entry .mro at path, compiling its invocation as mrp
// would. The declarations are the file as Martian's formatter prints it,
// less the call.
func ReadSource(path string, mroPaths []string) (Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Source{}, fmt.Errorf("read %s: %w", path, err)
	}

	var p syntax.Parser

	file, err := p.UncheckedParse(raw, path)
	if err != nil {
		return Source{}, fmt.Errorf("parse %s: %w", path, err)
	}

	call := (&syntax.Ast{Call: file.Call}).Format()
	file.Call = nil

	_, _, ast, err := syntax.Compile(path, mroPaths, false)
	if err != nil {
		return Source{}, fmt.Errorf("compile %s: %w", path, err)
	}

	if ast.Call == nil {
		return Source{}, fmt.Errorf("compile %s: %w", path, errNoCall)
	}

	inv, err := core.BuildDataForAst(ast)
	if err != nil {
		return Source{}, fmt.Errorf("invocation of %s: %w", path, err)
	}

	if _, err := graphOf(ast, path, mroPaths); err != nil {
		return Source{}, err
	}

	return Source{Name: filepath.Base(path), Decls: file.Format(), Call: call, Invocation: inv, dir: filepath.Dir(path)}, nil
}

// Rebuild is the invocation source of inv over the declarations: the call
// mrp's invocation generator builds from invocation data (core.BuildCallAst,
// as `mrg` runs it), after them.
func (s Source) Rebuild(inv *core.InvocationData, mroPaths []string) (string, error) {
	callable, lookup, err := s.callable(inv.Call, mroPaths)
	if err != nil {
		return "", err
	}

	args := inv.Args
	if args == nil {
		args = core.LazyArgumentMap{}
	}

	ast, err := core.BuildCallAst(inv.Call, args.ToMarshalerMap(), inv.SplitArgs, callable, lookup, mroPaths)
	if err != nil {
		return "", fmt.Errorf("build call of %s: %w", inv.Call, err)
	}

	return s.with((&syntax.Ast{Call: ast.Call}).Format()), nil
}

// Manifest is the type manifest of the source's declarations.
func (s Source) Manifest(mroPaths []string) (types.Manifest, error) {
	var p syntax.Parser

	_, _, ast, err := p.ParseSourceBytes([]byte(s.with(s.Call)), s.file(), mroPaths, false)
	if err != nil {
		return types.Manifest{}, fmt.Errorf("compile %s: %w", s.Name, err)
	}

	return types.BuildManifest(frontend.Lower(ast)), nil
}

// Witness compiles the witness invocation of the source's call, whose call
// graph holds every stage node, with its fork roots, of the call's
// invocations: every entry input given a value of its type that is no
// null, with witnessWidth elements in every collection, nested collections
// of different lengths so no nested split's length is uniform, and false
// for every bool, so no map call is pruned on an empty or null source and
// no call is disabled by an entry value. Where mrp refuses that invocation
// (it refuses, say, a runtime-gated split of a non-empty literal), each
// input in turn takes its witness value over the literal invocation's only
// while mrp still accepts the call.
func (s Source) Witness(mroPaths []string) (*Graph, error) {
	callable, lookup, err := s.callable(s.Invocation.Call, mroPaths)
	if err != nil {
		return nil, err
	}

	witness := map[string]json.RawMessage{}

	for _, p := range callable.GetInParams().List {
		v := witnessValue(lookup.Get(p.GetTname()), lookup, witnessWidth)
		if slices.Contains(s.Invocation.SplitArgs, p.GetId()) {
			v = append(append(json.RawMessage("["), v...), ']')
		}

		witness[p.GetId()] = v
	}

	full := core.LazyArgumentMap{}
	maps.Copy(full, witness)

	if g, err := s.compileArgs(full, mroPaths); err == nil {
		return g, nil
	}

	args := core.LazyArgumentMap{}
	maps.Copy(args, s.Invocation.Args)

	for _, p := range callable.GetInParams().List {
		was, had := args[p.GetId()]
		args[p.GetId()] = witness[p.GetId()]

		if _, err := s.compileArgs(args, mroPaths); err != nil {
			delete(args, p.GetId())

			if had {
				args[p.GetId()] = was
			}
		}
	}

	return s.compileArgs(args, mroPaths)
}

// with is the source with call after the declarations.
func (s Source) with(call string) string {
	return strings.TrimRight(s.Decls, "\n") + "\n\n" + call
}

// file is the path the declarations compile under.
func (s Source) file() string {
	return filepath.Join(s.dir, s.Name)
}

// callable compiles the entry file and returns the callable id and the
// type table it is declared with.
func (s Source) callable(id string, mroPaths []string) (syntax.Callable, *syntax.TypeLookup, error) {
	var p syntax.Parser

	_, _, ast, err := p.ParseSourceBytes([]byte(s.with(s.Call)), s.file(), mroPaths, false)
	if err != nil {
		return nil, nil, fmt.Errorf("compile the declarations of %s: %w", s.Name, err)
	}

	c := ast.Callables.Table[id]
	if c == nil {
		return nil, nil, fmt.Errorf("%w: %q in %s", errNoCallable, id, s.Name)
	}

	return c, &ast.TypeTable, nil
}

// compileArgs compiles the source's call with args.
func (s Source) compileArgs(args core.LazyArgumentMap, mroPaths []string) (*Graph, error) {
	inv := &core.InvocationData{Call: s.Invocation.Call, Args: args, SplitArgs: s.Invocation.SplitArgs}

	src, err := s.Rebuild(inv, mroPaths)
	if err != nil {
		return nil, fmt.Errorf("witness of %s: %w", s.Name, err)
	}

	return Compile([]byte(src), s.file(), mroPaths)
}

// witnessValue is the witness value of type t whose collections hold n
// elements, the i-th element's own collections i+1.
func witnessValue(t syntax.Type, lookup *syntax.TypeLookup, n int) json.RawMessage {
	switch t := t.(type) {
	case *syntax.ArrayType:
		inner := t.Elem
		if t.Dim > 1 {
			inner = lookup.GetArray(t.Elem, t.Dim-1)
		}

		return collection(n, func(i int) []byte { return witnessValue(inner, lookup, i+1) }, "[", "]", nil)
	case *syntax.TypedMapType:
		return collection(n, func(i int) []byte { return witnessValue(t.Elem, lookup, i+1) }, "{", "}", func(i int) string {
			return strconv.Quote(strconv.Itoa(i)) + ":"
		})
	case *syntax.StructType:
		var b strings.Builder

		b.WriteByte('{')

		for i, m := range t.Members {
			if i > 0 {
				b.WriteByte(',')
			}

			b.WriteString(strconv.Quote(m.Id) + ":")
			b.Write(witnessValue(lookup.Get(m.Tname), lookup, n))
		}

		b.WriteByte('}')

		return json.RawMessage(b.String())
	default:
		return scalarWitness(t)
	}
}

// collection writes n elements between open and close, each element i
// prefixed by key(i) when key is set.
func collection(n int, elem func(int) []byte, open, closing string, key func(int) string) json.RawMessage {
	var b strings.Builder

	b.WriteString(open)

	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}

		if key != nil {
			b.WriteString(key(i))
		}

		b.Write(elem(i))
	}

	b.WriteString(closing)

	return json.RawMessage(b.String())
}

// scalarWitness is the witness value of a type that is no collection or
// struct: a file type's value names a file, which no compile reads.
func scalarWitness(t syntax.Type) json.RawMessage {
	id := ""
	if t != nil {
		id = t.TypeId().Tname
	}

	switch id {
	case syntax.KindBool:
		return json.RawMessage("false")
	case syntax.KindInt:
		return json.RawMessage("0")
	case syntax.KindFloat:
		return json.RawMessage("0.5")
	case syntax.KindString:
		return json.RawMessage(`""`)
	case syntax.KindMap:
		return json.RawMessage("{}")
	default:
		return json.RawMessage(`"witness"`)
	}
}
