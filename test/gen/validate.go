package gen

import (
	"errors"
	"fmt"

	"github.com/martian-lang/martian/martian/syntax"
)

// errCompilerPanic reports source the Martian compiler panicked on instead
// of rejecting it.
var errCompilerPanic = errors.New("martian compiler panicked")

// Validate parses and compiles p's MRO with the real Martian compiler, the
// one mrp runs, so a kept program is exactly one Martian accepts.
func Validate(p *Program) error {
	if err := compileMRO(p.MRO()); err != nil {
		return fmt.Errorf("compile seed %d attempt %d: %w", p.Seed, p.Attempt, err)
	}

	return nil
}

// compileMRO compiles src, returning a compiler panic as an error: some
// shrink candidates crash the compiler (syntax CallStm.checkMappings on a
// projection of a missing pipeline output), and one crash would end every
// test sharing the process.
func compileMRO(src string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errCompilerPanic, r)
		}
	}()

	if _, _, _, err := syntax.ParseSourceBytes([]byte(src), "pipeline.mro", nil, false); err != nil {
		return fmt.Errorf("martian: %w", err)
	}

	return nil
}
