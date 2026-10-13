package cg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The records only mre reads: an instance stamp beside an args or outs
// bundle's data.json, the task record beside a posted outs bundle's stamp,
// the fork record of a fork element dir, and the record of a partner element
// dir (partnerJSON).
const (
	instanceFile = "instance.json"
	taskFile     = "task.json"
	elementFile  = "fork.json"
	partnerFile  = "partner.json"

	filePerm = 0o644
	dirPerm  = 0o755
)

var (
	errNotItem = errors.New("not an instance bundle, a fork element or a partner element")
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

// taskJSON is where a posted outs bundle was written: the dir of the task
// that posted it, where its stage ran, and the bundle's path below it. The
// pipeline outs name a path the stage declared in that dir, a file it never
// wrote, below the task's work dir, which a scratch dir is not (#457).
type taskJSON struct {
	Dir string `json:"dir"`
	At  string `json:"at"`
}

// stampPosted stamps the posted outs bundle dir with its instance and the
// task posting it, which runs in the dir its stage ran in.
func stampPosted(dir string, in Instance) error {
	if err := writeInstance(dir, in); err != nil {
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("task dir: %w", err)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("task dir: %w", err)
	}

	at, err := filepath.Rel(wd, abs)
	if err != nil {
		return fmt.Errorf("task dir: %w", err)
	}

	return writeRecord(filepath.Join(dir, taskFile), taskJSON{Dir: wd, At: filepath.ToSlash(at)})
}

// readTask reads the task record beside the posted outs bundle dir, and
// false for a bundle without one.
func readTask(dir string) (taskJSON, bool, error) {
	var rec taskJSON

	err := readRecord(filepath.Join(dir, taskFile), &rec)
	if errors.Is(err, fs.ErrNotExist) {
		return rec, false, nil
	}

	return rec, err == nil, err
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

	el, err := g.element(rec)
	if err != nil {
		return Element{}, fmt.Errorf("fork element %s: %w", dir, err)
	}

	return el, nil
}

// DecodeElement reads a fork record passed as a value: the base64 of its
// JSON, so a map key of any characters crosses a shell unquoted.
func (g *Graph) DecodeElement(text string) (Element, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return Element{}, fmt.Errorf("decode fork record: %w", err)
	}

	var rec elementJSON
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Element{}, fmt.Errorf("decode fork record: %w", err)
	}

	el, err := g.element(rec)
	if err != nil {
		return Element{}, fmt.Errorf("fork record: %w", err)
	}

	return el, nil
}

func (g *Graph) element(rec elementJSON) (Element, error) {
	f, err := g.decodeFork(rec.Fork)
	if err != nil {
		return Element{}, err
	}

	if len(f) == 0 {
		return Element{}, fmt.Errorf("%w: no parts", errBadFork)
	}

	if len(rec.Lens) != len(f) {
		return Element{}, fmt.Errorf("%w: %d counts, %d parts", errLens, len(rec.Lens), len(f))
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

// itemKind reports which record dir holds: instanceFile, elementFile,
// partnerFile, or "" for none.
func itemKind(dir string) string {
	for _, name := range []string{instanceFile, elementFile, partnerFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}

	return ""
}
