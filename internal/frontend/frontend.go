// Package frontend parses Martian MRO source into an AST using Martian's own
// syntax package, so the transpiler accepts exactly what the real compiler does.
package frontend

import (
	"fmt"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/martian-lang/martian/martian/syntax"
)

// Parse compiles the MRO file at path, resolving @include directives against
// mroPaths. When checkSrc is true it also verifies that each stage's src file
// exists on disk. On failure it returns an error wrapping apperror.ErrParse.
func Parse(path string, mroPaths []string, checkSrc bool) (*syntax.Ast, error) {
	_, _, ast, err := syntax.Compile(path, mroPaths, checkSrc)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", apperror.ErrParse, err)
	}

	return ast, nil
}
