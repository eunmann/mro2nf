package frontend

import (
	"fmt"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/martian-lang/martian/martian/syntax"
)

// CheckCallGraph resolves the invocation's call graph with the function mrp
// runs before it creates a pipestance (core/runtime.go instantiatePipeline), so a
// pipeline mrp refuses before any stage runs is refused here too, with mrp's
// message (#387). An AST without a call statement has no invocation to
// resolve. The resolution may rewrite parts of the AST, so it runs after
// Lower. On failure it returns an error wrapping apperror.ErrParse.
func CheckCallGraph(ast *syntax.Ast) error {
	if ast.Call == nil {
		return nil
	}

	if _, err := ast.MakePipelineCallGraph("", ast.Call); err != nil {
		return fmt.Errorf("%w: resolve call graph of %s: %w", apperror.ErrParse, ast.Call.DecId, err)
	}

	return nil
}
