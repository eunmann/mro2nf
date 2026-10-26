package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/cg"
)

// witnessSuffix marks the leg of a finding a program's witness invocation
// found.
const witnessSuffix = "@witness"

// witnessSource is the entry source of the fixture at dir at its witness
// invocation (cg.Source.Witness): every entry collection non-empty, nested
// ones of different lengths, where a generated program's literal
// invocation mostly binds empty collections. It is false when that is the
// fixture's own invocation.
func witnessSource(dir string) (string, bool, error) {
	mro, paths := filepath.Join(dir, "pipeline.mro"), []string{dir}

	src, err := cg.ReadSource(mro, paths)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", mro, err)
	}

	w, _, err := src.Witness(paths)
	if err != nil {
		return "", false, fmt.Errorf("witness of %s: %w", mro, err)
	}

	inv, err := w.Invocation()
	if err != nil {
		return "", false, fmt.Errorf("witness invocation of %s: %w", mro, err)
	}

	text, err := src.Rebuild(inv, paths)
	if err != nil {
		return "", false, fmt.Errorf("rebuild witness of %s: %w", mro, err)
	}

	own, err := src.Rebuild(src.Invocation, paths)
	if err != nil {
		return "", false, fmt.Errorf("rebuild %s: %w", mro, err)
	}

	return text, text != own, nil
}

// writeWitnessFixture writes at out the fixture at dir with entry source
// text.
func writeWitnessFixture(dir, out, text string) error {
	if err := os.CopyFS(out, os.DirFS(dir)); err != nil {
		return fmt.Errorf("copy fixture: %w", err)
	}

	if err := os.WriteFile(filepath.Join(out, "pipeline.mro"), []byte(text), 0o600); err != nil {
		return fmt.Errorf("write witness pipeline.mro: %w", err)
	}

	return nil
}

// mrpUnresolvedLog is what mrp logs when it fails to resolve a job's input
// bindings (core/stage.go:1402). At its default --strict=disable it runs
// the job anyway, the bindings it failed to resolve null (#606).
const mrpUnresolvedLog = "Error resolving input argument bindings"

// mrpRanUnresolved reports whether the mrp log at path shows a job run on
// bindings mrp failed to resolve, whose outputs are no ground truth.
func mrpRanUnresolved(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read mrp log: %w", err)
	}

	return strings.Contains(string(raw), mrpUnresolvedLog), nil
}

// joinVerdicts is a program's verdict from its runs at its literal and
// witness invocations: the witness run's findings under witness legs, and
// no test only where neither run is one.
func joinVerdicts(literal, witness genVerdict) genVerdict {
	out := genVerdict{
		findings: slices.Clip(literal.findings),
		notes:    append(slices.Clip(literal.notes), witness.notes...),
	}

	for _, f := range witness.findings {
		f.leg += witnessSuffix
		out.findings = append(out.findings, f)
	}

	switch {
	case literal.skip != "" && witness.skip != "":
		out.skip = "literal invocation: " + literal.skip + "\nwitness invocation: " + witness.skip
	case literal.skip != "":
		out.notes = append(out.notes, "literal invocation is no test: "+literal.skip)
	case witness.skip != "":
		out.notes = append(out.notes, "witness invocation is no test: "+witness.skip)
	}

	return out
}

// atWitness reports whether v's findings are all the witness run's, so its
// fixture reproduces them at the witness invocation.
func (v genVerdict) atWitness() bool {
	return len(v.findings) > 0 && !slices.ContainsFunc(v.findings, func(f genFinding) bool {
		return !strings.HasSuffix(f.leg, witnessSuffix)
	})
}
