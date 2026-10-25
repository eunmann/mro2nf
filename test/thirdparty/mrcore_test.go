package thirdparty

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mrcoreAdaptedFile is the file of internal/mrcore that holds the functions
// it adapts from core; its other copies are core's own source.
const mrcoreAdaptedFile = "adapted.go"

// mrcoreCopies are the files of internal/mrcore that copy Martian core.
func mrcoreCopies() []string {
	return []string{"fork.go", "resolve.go", "convert.go", mrcoreAdaptedFile}
}

// coreSources are the files of Martian core internal/mrcore copies from.
func coreSources() []string {
	return []string{
		"martian/core/fork.go", "martian/core/resolve.go", "martian/core/runtime.go",
		"martian/core/node.go", "martian/core/stage.go",
	}
}

// mrcoreAdapted are the functions internal/mrcore changes from core, each
// with why: everything else it copies is core's own source.
func mrcoreAdapted() map[string]string {
	return map[string]string{
		"(*Node).resolveInputs":          "a job's _args only (no keep-split), no read limit, a not-ready read fails",
		"(*TopNode).resolveMerge":        "getParts' errors are returned, not also printed",
		"(*TopNode).getParts":            "expands the bound node scoped to the fork (view), its table parts recomputed",
		"(*TopNode).resolveSplit":        "takes a staged or literal element without resolving the collection (splitElement)",
		"getArrayElement":                "an index past the end wraps ErrNoElement",
		"(*Node).matchFork":              "a determined fork id is the fork; others match within a scoped view",
		"(*Node).matchForks":             "matches within a scoped view",
		"(*Fork).resolveRef":             "reads the store; an unstaged read is not ready; Path's arrays as this package's",
		"(*Fork).expandForkPart":         "the fork's disabled flag in place of its metadata state",
		"(ForkId).getUnmatchedForkParts": "the bound node's forks scoped to the fork (view)",
		"(*Fork).expandForkFromRef":      "the bound node's forks scoped to the fork (view)",
		"(*Node).expandForks":            "an expansion error is kept on the node, not written to the fork's metadata",
		"(*Node).GetFQName":              "the node's fqid, with no pipestance name",
		"(*Fork).updateId":               "sets the fork id and a name keyed by it, with no pipestance paths",
		"(*Fork).writeDisable":           "marks the fork disabled; its outs read null (Fork.outs)",
	}
}

// funcSources maps each function declared in the Go file at path, by
// receiver and name, to its source without its doc comment.
func funcSources(path string) (map[string]string, error) {
	fset := token.NewFileSet()

	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := map[string]string{}

	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}

		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fn); err != nil {
			return nil, fmt.Errorf("print %s: %w", fn.Name.Name, err)
		}

		out[funcKey(fn)] = buf.String()
	}

	return out, nil
}

func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}

	recv := fn.Recv.List[0].Type
	star := ""

	if s, ok := recv.(*ast.StarExpr); ok {
		recv, star = s.X, "*"
	}

	name := "?"
	if id, ok := recv.(*ast.Ident); ok {
		name = id.Name
	}

	return "(" + star + name + ")." + fn.Name.Name
}

// coreFuncs are the functions of the core source files in moduleDir.
func coreFuncs(moduleDir string) (map[string]string, error) {
	out := map[string]string{}

	for _, src := range coreSources() {
		fns, err := funcSources(filepath.Join(moduleDir, filepath.FromSlash(src)))
		if err != nil {
			return nil, err
		}

		maps.Copy(out, fns)
	}

	return out, nil
}

// mrcoreDrift lists how the copied files of internal/mrcore differ from the
// core source in moduleDir beyond the declared adaptations: a function core
// does not declare, one that differs undeclared or outside the adapted
// file, or a declared adaptation that no longer differs.
func mrcoreDrift(mrcoreDir, moduleDir string) ([]string, error) {
	theirs, err := coreFuncs(moduleDir)
	if err != nil {
		return nil, err
	}

	var drift []string

	seen := map[string]bool{}
	adapted := mrcoreAdapted()

	for _, name := range mrcoreCopies() {
		ours, err := funcSources(filepath.Join(mrcoreDir, name))
		if err != nil {
			return nil, err
		}

		for _, k := range slices.Sorted(maps.Keys(ours)) {
			seen[k] = true

			core, ok := theirs[k]
			_, declared := adapted[k]

			switch {
			case !ok:
				drift = append(drift, name+": "+k+" is not core's; mro2nf's own code belongs outside the copies")
			case core != ours[k] && !declared:
				drift = append(drift, name+": "+k+" differs from core undeclared")
			case core == ours[k] && declared:
				drift = append(drift, name+": "+k+" is declared adapted but matches core")
			case declared && name != mrcoreAdaptedFile:
				drift = append(drift, name+": "+k+" is adapted outside "+mrcoreAdaptedFile)
			}
		}
	}

	for _, k := range slices.Sorted(maps.Keys(adapted)) {
		if !seen[k] {
			drift = append(drift, k+" is declared adapted but not copied")
		}
	}

	return drift, nil
}

// TestMrcoreDrift: the check names an undeclared change to a copied
// function, a function core does not declare, and a declared adaptation
// that matches core again.
func TestMrcoreDrift(t *testing.T) {
	ours, module := t.TempDir(), t.TempDir()

	write := func(dir, name, src string) {
		t.Helper()

		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, src := range coreSources() {
		write(module, src, "package core\n")
	}

	for _, name := range mrcoreCopies() {
		write(ours, name, "package mrcore\n")
	}

	write(module, "martian/core/resolve.go", "package core\n\nfunc getElement() int { return 1 }\n\nfunc getArrayElement() int { return 1 }\n\n"+
		"func (n *Node) matchFork() int { return 1 }\n")
	write(ours, "resolve.go", "package mrcore\n\nfunc getElement() int { return 2 }\n\nfunc mine() {}\n\n"+
		"func (n *Node) matchFork() int { return 2 }\n")
	write(ours, mrcoreAdaptedFile, "package mrcore\n\nfunc getArrayElement() int { return 1 }\n")

	drift, err := mrcoreDrift(ours, module)
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(drift, "\n")
	for _, want := range []string{
		"getElement differs from core undeclared",
		"mine is not core's",
		"getArrayElement is declared adapted but matches core",
		"(*Node).matchFork is adapted outside adapted.go",
		"(*TopNode).getParts is declared adapted but not copied",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("drift lacks %q:\n%s", want, got)
		}
	}
}

// TestMrcoreMatchesCore fails, naming each function, when internal/mrcore's
// copies of Martian core's runtime resolution drift from the core go.mod
// pins beyond the adaptations it declares, so a pin bump shows what core
// changed in the logic mro2nf runs.
func TestMrcoreMatchesCore(t *testing.T) {
	dir, err := ModuleDir(t.Context(), repoRoot, Vendored()[vendorMartian])
	if err != nil {
		t.Fatal(err)
	}

	drift, err := mrcoreDrift(filepath.Join(repoRoot, "internal", "mrcore"), dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(drift) > 0 {
		t.Errorf("internal/mrcore drifted from %s:\n%s", dir, strings.Join(drift, "\n"))
	}
}
