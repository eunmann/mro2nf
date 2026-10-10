package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	// errGatherCount reports a gather whose staged entries do not number what
	// the producer's sidecar declares: a lost or extra fork/chunk bundle must
	// fail the task, never shorten (or pad) the gathered result.
	errGatherCount = errors.New("gathered entry count differs from the expected count")
	// errGatherEntry reports a staged entry whose name does not carry the
	// index its producer encodes, or that collides with another entry.
	errGatherEntry = errors.New("malformed gather entry")
)

// gatherIndexed returns the paths of dir's entries named prefix+<stem><index>,
// ordered by index, after checking they are exactly indices 0..want-1 under one
// shared stem. Every gathered name ends in the decimal index its writer
// assigned (fork_%05d, chunk_%05d, a composite outer~fork_%05d key), so the
// index, not a name sort, is the order: it is the order the sidecar's keys and
// chunk defs are listed in, and it needs no padding width or locale. A missing
// dir is zero entries, which is what Nextflow stages for an empty list.
func gatherIndexed(dir, prefix string, want int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("list gather dir %s: %w", dir, err)
	}

	names := make([]string, 0, want)

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}

	if len(names) != want {
		return nil, fmt.Errorf("%w: %d %s* entries in %s, want %d", errGatherCount, len(names), prefix, dir, want)
	}

	return orderByIndex(dir, names)
}

// orderByIndex places each name at its trailing index. The caller has already
// matched the count, so a dense, duplicate-free index set is a permutation of
// 0..len(names)-1.
func orderByIndex(dir string, names []string) ([]string, error) {
	paths := make([]string, len(names))
	stem := ""

	for i, name := range names {
		s, idx, err := splitIndex(name)
		if err != nil {
			return nil, err
		}

		if i == 0 {
			stem = s
		} else if s != stem {
			return nil, fmt.Errorf("%w: %q does not share the stem %q", errGatherEntry, name, stem)
		}

		if idx >= len(paths) {
			return nil, fmt.Errorf("%w: %q index %d is past the %d expected entries", errGatherEntry, name, idx, len(paths))
		}

		if paths[idx] != "" {
			return nil, fmt.Errorf("%w: %q repeats index %d of %q", errGatherEntry, name, idx, filepath.Base(paths[idx]))
		}

		paths[idx] = filepath.Join(dir, name)
	}

	return paths, nil
}

// splitIndex splits a name into its stem and its trailing decimal index.
func splitIndex(name string) (string, int, error) {
	cut := len(name)
	for cut > 0 && name[cut-1] >= '0' && name[cut-1] <= '9' {
		cut--
	}

	if cut == len(name) {
		return "", 0, fmt.Errorf("%w: %q has no trailing index", errGatherEntry, name)
	}

	idx, err := strconv.Atoi(name[cut:])
	if err != nil {
		return "", 0, fmt.Errorf("%w: %q index: %w", errGatherEntry, name, err)
	}

	return name[:cut], idx, nil
}
