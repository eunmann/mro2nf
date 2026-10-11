package gen

import (
	"fmt"

	"github.com/martian-lang/martian/martian/syntax"
)

// Validate parses and compiles p's MRO with the real Martian compiler, the
// one mrp runs, so a kept program is exactly one Martian accepts.
func Validate(p *Program) error {
	if _, _, _, err := syntax.ParseSourceBytes([]byte(p.MRO()), "pipeline.mro", nil, false); err != nil {
		return fmt.Errorf("compile seed %d attempt %d: %w", p.Seed, p.Attempt, err)
	}

	return nil
}
