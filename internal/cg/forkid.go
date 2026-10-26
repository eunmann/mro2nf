package cg

import (
	"fmt"

	"github.com/eunmann/mro2nf/internal/mrcore"
)

// ForkID is the id mrp names node's fork f by, its fork dir's and its jobs'
// (core/fork.go:1010 ForkId.ForkIdString), lens[i] the number of forks of
// f[i]'s root under the parts before it (Instance.Lens).
func (g *Graph) ForkID(node string, f Fork, lens []int) (string, error) {
	cf, err := countedFork(f, lens)
	if err != nil {
		return "", err
	}

	r := &resolver{g: g, st: emptyStore{}}

	id, err := r.core().ForkID(node, cf)
	if err != nil {
		return "", fmt.Errorf("fork id: %w", err)
	}

	return id, nil
}

// countedFork is f as a core fork id whose parts carry the fork counts lens
// as their ranges (mrcore.CountedPart).
func countedFork(f Fork, lens []int) (mrcore.ForkId, error) {
	if len(lens) != len(f) {
		return nil, fmt.Errorf("%w: %d counts, %d parts", errLens, len(lens), len(f))
	}

	if len(f) == 0 {
		return nil, nil
	}

	out := make(mrcore.ForkId, len(f))
	for i, p := range f {
		out[i] = mrcore.CountedPart(p.Root, p.Index, lens[i])
	}

	return out, nil
}
