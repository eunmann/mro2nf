package gen

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

var (
	errNoCall            = errors.New("holds no call")
	errStageDirCollision = errors.New("stages share a stub dir")
	errTopologyChanged   = errors.New("the stub fixture's call graph differs from the invocation's")
)

// WriteStubFixture writes at dir, in the layout WriteFixture writes, the
// invocation at mro, compiled with the include path mroPath, with every
// stage's code the stub drawing from seed. pipeline.mro is the compiled
// invocation as Martian's formatter prints it: one file, every declaration
// of its includes in it, each stage's src repointed at its stub at
// StageDir(id), whose spec.json holds the stage's compiled parameter types.
// The rest is the invocation's own: its bindings, map calls, disabled
// bindings, splits, retains, resources and modifiers. It fails when Martian
// compiles the written file to another call graph or other declarations.
func WriteStubFixture(dir, mro string, mroPath []string, seed uint64) error {
	ast, err := compileStubbed(mro, mroPath)
	if err != nil {
		return err
	}

	if ast.Call == nil {
		return fmt.Errorf("%s: %w", mro, errNoCall)
	}

	if err := writeStubs(dir, ast, seed); err != nil {
		return err
	}

	want, err := topologyOf(ast)
	if err != nil {
		return fmt.Errorf("%s: %w", mro, err)
	}

	path := filepath.Join(dir, "pipeline.mro")
	if err := os.WriteFile(path, []byte(ast.Format()), filePerm); err != nil {
		return fmt.Errorf("write pipeline.mro: %w", err)
	}

	written, err := compileStubbed(path, []string{dir})
	if err != nil {
		return err
	}

	got, err := topologyOf(written)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	if got != want {
		return fmt.Errorf("%s from %s: %w (-invocation +stub fixture):\n%s", path, mro, errTopologyChanged, cmp.Diff(want, got))
	}

	return nil
}

// compileStubbed compiles the invocation at path with the include path
// mroPath, as mrp does, a compiler panic returned as an error.
func compileStubbed(path string, mroPath []string) (*syntax.Ast, error) {
	var ast *syntax.Ast

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%w: %v", errCompilerPanic, r)
			}
		}()

		_, _, ast, err = syntax.Compile(path, mroPath, false)
		if err != nil {
			return fmt.Errorf("martian: %w", err)
		}

		return nil
	}()
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", path, err)
	}

	return ast, nil
}

// writeStubs writes the stub of every stage ast declares below dir and
// repoints the stage's src at it.
func writeStubs(dir string, ast *syntax.Ast, seed uint64) error {
	structs, fileTypes := astStructSpecs(ast), astFileTypes(ast)
	owners := map[string]string{}

	for _, st := range ast.Stages {
		rel := StageDir(st.Id)
		if prev, ok := owners[rel]; ok {
			return fmt.Errorf("%w: %s and %s at %s", errStageDirCollision, prev, st.Id, rel)
		}

		owners[rel] = st.Id

		err := writeStub(filepath.Join(dir, filepath.FromSlash(rel)), stubSpec{
			Name:      st.Id,
			In:        inSpecs(st.InParams),
			Out:       outSpecs(st.OutParams),
			Split:     st.Split,
			ChunkIn:   inSpecs(st.ChunkIns),
			ChunkOut:  outSpecs(st.ChunkOuts),
			Structs:   structs,
			FileTypes: fileTypes,
			Seed:      seed,
		})
		if err != nil {
			return fmt.Errorf("stage %s: %w", st.Id, err)
		}

		st.Src = &syntax.SrcParam{Node: st.Src.Node, Lang: "py", Path: rel}
	}

	return nil
}

// astStructSpecs maps every struct type a value of the compiled ast may
// carry to its fields: the declared structs, and each callable, whose
// outputs are its struct.
func astStructSpecs(ast *syntax.Ast) map[string][][2]string {
	out := map[string][][2]string{}

	for _, s := range ast.StructTypes {
		out[s.Id] = memberSpecs(s.Members)
	}

	for _, c := range ast.Callables.List {
		out[c.GetId()] = outSpecs(c.GetOutParams())
	}

	return out
}

// astFileTypes are the user file types ast declares, sorted.
func astFileTypes(ast *syntax.Ast) []string {
	out := make([]string, 0, len(ast.UserTypes))
	for _, u := range ast.UserTypes {
		out = append(out, u.Id)
	}

	slices.Sort(out)

	return slices.Compact(out)
}

func inSpecs(ps *syntax.InParams) [][2]string {
	if ps == nil {
		return [][2]string{}
	}

	out := make([][2]string, len(ps.List))
	for i, p := range ps.List {
		out[i] = [2]string{p.Id, p.Tname.String()}
	}

	return out
}

func outSpecs(ps *syntax.OutParams) [][2]string {
	if ps == nil {
		return [][2]string{}
	}

	out := make([][2]string, len(ps.List))
	for i, p := range ps.List {
		out[i] = [2]string{p.Id, p.Tname.String()}
	}

	return out
}

func memberSpecs(ms []*syntax.StructMember) [][2]string {
	out := make([][2]string, len(ms))
	for i, m := range ms {
		out[i] = [2]string{m.Id, m.Tname.String()}
	}

	return out
}

// topologyOf is what a stub fixture must keep of its invocation: Martian's
// call graph of its call, which holds every node's resolved bindings,
// disabled bindings, fork roots and retains, and every declaration as
// Martian's formatter prints it, which holds the splits, resources and call
// modifiers the graph does not. Comments and blank lines are dropped: the
// formatter prints a compiled ast's includes as comments.
func topologyOf(ast *syntax.Ast) (string, error) {
	g, err := ast.MakePipelineCallGraph("", ast.Call)
	if err != nil {
		return "", fmt.Errorf("call graph: %w", err)
	}

	graph, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("encode call graph: %w", err)
	}

	decls := (&syntax.Ast{
		UserTypes: ast.UserTypes, StructTypes: ast.StructTypes, Callables: ast.Callables, Call: ast.Call,
	}).Format()

	var b strings.Builder

	b.Write(graph)

	for line := range strings.Lines(decls) {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
			b.WriteString("\n" + strings.TrimRight(line, "\n"))
		}
	}

	return b.String(), nil
}
