package cg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

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
// invocation, as `mrg --reverse` writes it). Name and Dir are the entry
// file's name and absolute dir: every compile of the source, and of an
// invocation rebuilt from it, is a compile of the file at Path, as mrp
// compiles the invocation under the path it was given, so Martian resolves
// the entry's includes and its stages' code beside the entry before the
// search path, and _jobinfo records the include mrp records. Files are the
// absolute paths of every .mro file the entry compiles from, itself and its
// includes, sorted.
type Source struct {
	Name       string
	Dir        string
	Decls      string
	Call       string
	Invocation *core.InvocationData
	Files      []string
}

// ReadSource reads the entry .mro at path, compiling its invocation as mrp
// would. The declarations are the file as Martian's formatter prints it,
// less the call.
func ReadSource(path string, mroPaths []string) (Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Source{}, fmt.Errorf("read %s: %w", path, err)
	}

	decls, call, err := splitEntry(raw, path, mroPaths)
	if err != nil {
		return Source{}, err
	}

	ast, err := compileEntry(path, mroPaths)
	if err != nil {
		return Source{}, err
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

	abs, err := filepath.Abs(path)
	if err != nil {
		return Source{}, fmt.Errorf("resolve %s: %w", path, err)
	}

	return Source{
		Name: filepath.Base(abs), Dir: filepath.Dir(abs), Decls: decls, Call: call, Invocation: inv,
		Files: slices.Sorted(maps.Keys(ast.Files)),
	}, nil
}

// splitEntry splits the entry .mro src, read from path, into its
// declarations and its call, each as Martian's formatter prints it.
func splitEntry(src []byte, path string, mroPaths []string) (string, string, error) {
	file, lit, err := parseMRO(src, path)
	if lit != "" {
		return "", "", literalError(path, mroPaths, lit)
	}

	if err != nil {
		return "", "", err
	}

	call := keepNegativeZero((&syntax.Ast{Call: file.Call}).Format())
	file.Call = nil

	return keepNegativeZero(file.Format()), call, nil
}

// compileEntry compiles the entry .mro at path with its includes.
func compileEntry(path string, mroPaths []string) (*syntax.Ast, error) {
	var ast *syntax.Ast

	lit, err := recoverLiteral(func() error {
		_, _, a, err := syntax.Compile(path, mroPaths, false)
		if err != nil {
			return fmt.Errorf("compile %s: %w", path, err)
		}

		ast = a

		return nil
	})
	if lit != "" {
		return nil, literalError(path, mroPaths, lit)
	}

	return ast, err
}

// Rebuild is the invocation source of inv over the declarations: the call
// mrp's invocation generator builds from invocation data (core.BuildCallAst,
// as `mrg` runs it), after them (RebuildCall).
func (s Source) Rebuild(inv *core.InvocationData, mroPaths []string) (string, error) {
	call, err := s.RebuildCall(inv, mroPaths)
	if err != nil {
		return "", err
	}

	return withCall(s.Decls, call), nil
}

// RebuildCall is the call mrp's invocation generator builds from inv over
// the declarations. An argument inv keeps from the source's own invocation
// keeps its float -0.0 (keepNegativeZero).
func (s Source) RebuildCall(inv *core.InvocationData, mroPaths []string) (string, error) {
	callable, lookup, err := s.callable(inv.Call, mroPaths)
	if err != nil {
		return "", err
	}

	args := make(core.LazyArgumentMap, len(inv.Args))
	for k, v := range inv.Args {
		if own, ok := s.Invocation.Args[k]; ok && bytes.Equal(own, v) {
			v = json.RawMessage(keepNegativeZero(string(v)))
		}

		args[k] = v
	}

	ast, err := core.BuildCallAst(inv.Call, args.ToMarshalerMap(), inv.SplitArgs, callable, lookup, mroPaths)
	if err != nil {
		return "", fmt.Errorf("build call of %s: %w", inv.Call, err)
	}

	return keepNegativeZero((&syntax.Ast{Call: ast.Call}).Format()), nil
}

// InParams are the input params of the source's callable id and the walk
// over the types they are declared with.
func (s Source) InParams(id string, mroPaths []string) ([]*syntax.StructMember, *types.Table, error) {
	c, lookup, err := s.callable(id, mroPaths)
	if err != nil {
		return nil, nil, err
	}

	ps, err := types.Params(c, types.RoleIn)
	if err != nil {
		return nil, nil, fmt.Errorf("params of %s: %w", id, err)
	}

	return ps, types.NewTable(lookup), nil
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

// CoversLiteral checks that t, the topology of the source's witness, covers
// the call graph of the source's own invocation, so a project fails its
// transpile, not its first run, where it cannot run the invocation it was
// transpiled from.
func (s Source) CoversLiteral(t Topology, mroPaths []string) error {
	g, err := s.compileArgs(s.Invocation.Args, mroPaths)
	if err != nil {
		return err
	}

	lt, err := g.Topology()
	if err != nil {
		return err
	}

	if err := t.Covers(lt); err != nil {
		return fmt.Errorf("the call of %s: %w", s.Name, err)
	}

	return nil
}

// Path is the entry file's path, which the source compiles as.
func (s Source) Path() string {
	return filepath.Join(s.Dir, s.Name)
}

// with is the source with call after the declarations.
func (s Source) with(call string) string {
	return withCall(s.Decls, call)
}

// withCall is the invocation source of call after the declarations decls.
func withCall(decls, call string) string {
	return strings.TrimRight(decls, "\n") + "\n\n" + call
}

// callable compiles the entry file and returns the callable id and the
// type table it is declared with.
func (s Source) callable(id string, mroPaths []string) (syntax.Callable, *syntax.TypeLookup, error) {
	var p syntax.Parser

	_, _, ast, err := p.ParseSourceBytes([]byte(s.with(s.Call)), s.Path(), mroPaths, false)
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

	return Compile([]byte(src), s.Path(), mroPaths)
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
