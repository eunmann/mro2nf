package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/shim"
)

// errNoLocation reports a file publish-layout leaves unpublished whose task's
// copy it cannot name.
var errNoLocation = errors.New("no location for an unpublished file")

// stagedLocations is the -locations document Mro2nf.leafLocations writes on
// the head node, where Nextflow knows what a task cannot: the work dir, and
// the URI each input item of the task was staged from, keyed by the item's
// path below the task dir. A task that runs in a scratch dir, as every task
// with an object-store work dir does, never sees its own work-dir URI.
type stagedLocations struct {
	Work   string            `json:"work"`
	Staged map[string]string `json:"staged"`
}

// locator names a file publish-layout leaves unpublished by where its task
// wrote it.
type locator struct {
	// leaves is the absolute dir of the staged leaves, task this task's dir.
	leaves, task string
	work         string
	staged       map[string]string
	// byOrigin maps the origin of each leaf below the staged input items to
	// the URI of its copy there, built on first use.
	byOrigin map[string]string
}

// newLocator returns the locator over the leaves staged in leavesDir and the
// -locations file, nil without leaves.
func newLocator(leavesDir, locationsFile string) (*locator, error) {
	if leavesDir == "" {
		return nil, nil
	}

	task, err := filepath.Abs(".")
	if err != nil {
		return nil, fmt.Errorf("resolve task dir: %w", err)
	}

	l := &locator{leaves: filepath.Join(task, leavesDir), task: task}
	if filepath.IsAbs(leavesDir) {
		l.leaves = leavesDir
	}

	if locationsFile == "" {
		return l, nil
	}

	raw, err := readFile(locationsFile)
	if err != nil {
		return nil, err
	}

	var doc stagedLocations
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode locations %s: %w", locationsFile, err)
	}

	l.work, l.staged = doc.Work, doc.Staged

	return l, nil
}

// locate names the leaf at leafPath below the staged leaves by its origin,
// the path its producing task wrote it at, when that lies in the work dir.
// Otherwise, as for a task that ran in a scratch dir, it names the URI of the
// copy Nextflow staged into this task, else the origin. A path in this task's
// own dir names a copy of the file no other task holds, so it is never the
// name.
func (l *locator) locate(leafPath string) (string, error) {
	if l == nil {
		return "", fmt.Errorf("%w %s: no staged leaves", errNoLocation, leafPath)
	}

	origin, err := shim.LeafOrigin(filepath.Join(l.leaves, filepath.FromSlash(leafPath)))
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", leafPath, err)
	}

	if l.work != "" && strings.HasPrefix(origin, strings.TrimSuffix(l.work, "/")+"/") && !l.inTask(origin) {
		return origin, nil
	}

	copied, err := l.stagedCopy(origin)
	if err != nil {
		return "", err
	}

	switch {
	case copied != "":
		return copied, nil
	case !l.inTask(origin):
		return origin, nil
	default:
		return "", fmt.Errorf("%w %s", errNoLocation, leafPath)
	}
}

func (l *locator) inTask(path string) bool {
	return strings.HasPrefix(path, l.task+string(filepath.Separator))
}

// stagedCopy returns the URI of the copy of the leaf of origin that Nextflow
// staged into this task, or "" when no input item holds one.
func (l *locator) stagedCopy(origin string) (string, error) {
	if l.byOrigin == nil {
		idx, err := indexStaged(l.task, l.staged)
		if err != nil {
			return "", err
		}

		l.byOrigin = idx
	}

	return l.byOrigin[origin], nil
}

// indexStaged maps the origin of each leaf below the staged input items of
// the task dir to the URI of its copy. Items are read in sorted order and the
// first copy of an origin is kept, so every run names the same one.
func indexStaged(task string, staged map[string]string) (map[string]string, error) {
	idx := map[string]string{}

	for _, item := range slices.Sorted(maps.Keys(staged)) {
		dir := filepath.Join(task, filepath.FromSlash(item))

		leaves, err := shim.StagedLeaves(dir)
		if err != nil {
			return nil, fmt.Errorf("index staged input %s: %w", item, err)
		}

		for _, leaf := range leaves {
			origin, err := shim.LeafOrigin(leaf)
			if err != nil {
				return nil, fmt.Errorf("index staged input %s: %w", item, err)
			}

			if _, ok := idx[origin]; ok {
				continue
			}

			sub, err := filepath.Rel(dir, leaf)
			if err != nil {
				return nil, fmt.Errorf("index staged input %s: %w", item, err)
			}

			idx[origin] = strings.TrimSuffix(staged[item], "/") + "/" + filepath.ToSlash(sub)
		}
	}

	return idx, nil
}
