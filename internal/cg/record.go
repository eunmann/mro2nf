package cg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The records only mre reads: an instance stamp beside an args or outs
// bundle's data.json, and the fork record of a fork element dir.
const (
	instanceFile = "instance.json"
	elementFile  = "fork.json"

	filePerm = 0o644
	dirPerm  = 0o755
)

var (
	errNotItem = errors.New("neither an instance bundle nor a fork element")
	errLens    = errors.New("fork counts do not match the fork's parts")
)

// Instance identifies one stage instance: its node, its fork, and whether
// it is disabled. Lens are the fork counts of its parts (Fork.ID).
type Instance struct {
	Node     string
	Fork     Fork
	Lens     []int
	Disabled bool
}

type instanceJSON struct {
	Node     string     `json:"node"`
	Fork     []partJSON `json:"fork"`
	Lens     []int      `json:"lens"`
	Disabled bool       `json:"disabled"`
}

// ForkID is the instance's fork id as mrp names the fork (Fork.ID).
func (in Instance) ForkID() string { return in.Fork.ID(in.Lens) }

// Element is one fork of a runtime fork root: its fork (the root's outer
// parts, then the root's own), the fork counts of those parts (Fork.ID),
// and its position among the root's forks under its outer parts, in mrp's
// order.
type Element struct {
	Fork     Fork
	Lens     []int
	Position int
}

type elementJSON struct {
	Fork     []partJSON `json:"fork"`
	Lens     []int      `json:"lens"`
	Position int        `json:"position"`
}

// ReadInstance reads the instance stamp in dir, an args or outs bundle.
func (g *Graph) ReadInstance(dir string) (Instance, error) {
	var rec instanceJSON
	if err := readRecord(filepath.Join(dir, instanceFile), &rec); err != nil {
		return Instance{}, err
	}

	if _, err := g.Stage(rec.Node); err != nil {
		return Instance{}, fmt.Errorf("instance stamp in %s: %w", dir, err)
	}

	f, err := g.decodeFork(rec.Fork)
	if err != nil {
		return Instance{}, fmt.Errorf("instance stamp in %s: %w", dir, err)
	}

	if len(rec.Lens) != len(f) {
		return Instance{}, fmt.Errorf("instance stamp in %s: %w: %d counts, %d parts", dir, errLens, len(rec.Lens), len(f))
	}

	return Instance{Node: rec.Node, Fork: f, Lens: rec.Lens, Disabled: rec.Disabled}, nil
}

func writeInstance(dir string, in Instance) error {
	return writeRecord(filepath.Join(dir, instanceFile), instanceJSON{
		Node: in.Node, Fork: in.Fork.encode(), Lens: orEmpty(in.Lens), Disabled: in.Disabled,
	})
}

func orEmpty(lens []int) []int {
	if lens == nil {
		return []int{}
	}

	return lens
}

// ReadElement reads the fork record of the fork element dir.
func (g *Graph) ReadElement(dir string) (Element, error) {
	var rec elementJSON
	if err := readRecord(filepath.Join(dir, elementFile), &rec); err != nil {
		return Element{}, err
	}

	f, err := g.decodeFork(rec.Fork)
	if err != nil {
		return Element{}, fmt.Errorf("fork element %s: %w", dir, err)
	}

	if len(f) == 0 {
		return Element{}, fmt.Errorf("fork element %s: %w: no parts", dir, errBadFork)
	}

	if len(rec.Lens) != len(f) {
		return Element{}, fmt.Errorf("fork element %s: %w: %d counts, %d parts", dir, errLens, len(rec.Lens), len(f))
	}

	return Element{Fork: f, Lens: rec.Lens, Position: rec.Position}, nil
}

func writeElement(dir string, el Element) error {
	return writeRecord(filepath.Join(dir, elementFile), elementJSON{Fork: el.Fork.encode(), Lens: el.Lens, Position: el.Position})
}

func readRecord(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}

func writeRecord(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, raw, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// itemKind reports which record dir holds: instanceFile, elementFile, or ""
// for neither.
func itemKind(dir string) string {
	for _, name := range []string{instanceFile, elementFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}

	return ""
}
