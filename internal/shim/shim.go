// Package shim runs a single phase (split, main, or join) of a Martian stage
// against the original Martian adapter, reproducing the on-disk ABI that
// mrp/mrjob provide. It lets generated Nextflow processes execute unmodified
// Martian stage code.
package shim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/eunmann/martian-nextflow/internal/apperror"
	"github.com/eunmann/martian-nextflow/internal/ir"
)

const (
	defaultPython = "python3"
	// extraVMemGB matches jobmanagers/config.json extra_vmem_per_job: the
	// default virtual-memory headroom added on top of the memory request.
	extraVMemGB = 3
	filePerm    = 0o644
	dirPerm     = 0o755
	disableFlag = "disable"
)

// errStageFailed indicates the stage code reported an error via the adapter's
// error channel (fd 4).
var errStageFailed = errors.New("stage failed")

// Adapter locates the stage code and the Martian adapter that runs it.
type Adapter struct {
	// Lang is the stage adapter language (only py is supported so far).
	Lang ir.Lang
	// Shell is the path to martian_shell.py.
	Shell string
	// Stagecode is the path to the stage module or binary.
	Stagecode string
	// SrcArgs are extra args from the stage's src declaration.
	SrcArgs []string
	// Python is the interpreter to use for py stages; defaults to python3.
	Python string
	// Mrjob is the path to the mrjob wrapper used to run comp stages.
	Mrjob string
}

// Resources is the per-phase resource allocation surfaced to stage code.
type Resources struct {
	Threads float64 `json:"threads"`
	MemGB   float64 `json:"mem_gb"`
	VMemGB  float64 `json:"vmem_gb"`
}

// Invocation is the minimal pipeline invocation recorded in _jobinfo.
type Invocation struct {
	Call    string
	Args    json.RawMessage
	MROFile string
}

// ChunkDef is one chunk produced by a stage's split phase: its per-chunk input
// args plus any resource overrides.
type ChunkDef struct {
	Args      map[string]json.RawMessage `json:"args"`
	Resources Resources                  `json:"resources"`
}

// RunSplit runs the split phase and returns the chunk definitions.
func RunSplit(
	ctx context.Context, workDir string, a Adapter,
	stageArgs json.RawMessage, res Resources, inv Invocation,
) ([]ChunkDef, error) {
	meta, files, journal, err := prepDirs(workDir, "split")
	if err != nil {
		return nil, err
	}

	if err := writeRaw(filepath.Join(meta, "_args"), orEmptyObj(stageArgs)); err != nil {
		return nil, err
	}

	if err := writeJobInfo(meta, files, "split", res, inv); err != nil {
		return nil, err
	}

	if err := runAdapter(ctx, meta, files, journal, a, "split"); err != nil {
		return nil, err
	}

	return readStageDefs(meta)
}

// RunMain runs one chunk's main phase and returns that chunk's _outs.
func RunMain(
	ctx context.Context, workDir string, a Adapter, stageArgs json.RawMessage,
	chunk ChunkDef, outNames []string, res Resources, inv Invocation,
) (json.RawMessage, error) {
	meta, files, journal, err := prepDirs(workDir, "main")
	if err != nil {
		return nil, err
	}

	args, err := mergeArgs(stageArgs, chunk, res)
	if err != nil {
		return nil, err
	}

	if err := stageInputs(meta, files, args, outNames, res, inv, "main"); err != nil {
		return nil, err
	}

	if err := runAdapter(ctx, meta, files, journal, a, "main"); err != nil {
		return nil, err
	}

	return readRaw(filepath.Join(meta, "_outs"))
}

// RunJoin runs the join phase and returns the stage's final _outs. chunkDefs
// and chunkOuts must be in matching chunk order.
func RunJoin(
	ctx context.Context, workDir string, a Adapter, stageArgs json.RawMessage,
	chunkDefs []ChunkDef, chunkOuts []json.RawMessage,
	outNames []string, res Resources, inv Invocation,
) (json.RawMessage, error) {
	meta, files, journal, err := prepDirs(workDir, "join")
	if err != nil {
		return nil, err
	}

	args, err := withResources(stageArgs, res)
	if err != nil {
		return nil, err
	}

	if err := stageInputs(meta, files, args, outNames, res, inv, "join"); err != nil {
		return nil, err
	}

	if err := writeChunkData(meta, chunkDefs, chunkOuts); err != nil {
		return nil, err
	}

	if err := runAdapter(ctx, meta, files, journal, a, "join"); err != nil {
		return nil, err
	}

	return readRaw(filepath.Join(meta, "_outs"))
}

// stageInputs writes the per-phase _args, _jobinfo, and skeleton _outs.
func stageInputs(
	meta, files string, args json.RawMessage,
	outNames []string, res Resources, inv Invocation, phase string,
) error {
	if err := writeRaw(filepath.Join(meta, "_args"), args); err != nil {
		return err
	}

	if err := writeJobInfo(meta, files, phase, res, inv); err != nil {
		return err
	}

	return writeSkeletonOuts(meta, outNames)
}

func writeChunkData(meta string, defs []ChunkDef, outs []json.RawMessage) error {
	args := make([]map[string]json.RawMessage, 0, len(defs))
	for _, d := range defs {
		args = append(args, d.Args)
	}

	if err := writeJSON(filepath.Join(meta, "_chunk_defs"), args); err != nil {
		return err
	}

	if outs == nil {
		outs = []json.RawMessage{}
	}

	return writeJSON(filepath.Join(meta, "_chunk_outs"), outs)
}

// runAdapter invokes the Martian adapter for one phase, dispatching by language.
func runAdapter(ctx context.Context, meta, files, journal string, a Adapter, phase string) error {
	switch a.Lang {
	case ir.LangPy:
		return runPyAdapter(ctx, meta, files, journal, a, phase)
	case ir.LangComp:
		if a.Mrjob == "" {
			return &apperror.UnsupportedError{Construct: "comp adapter", Detail: "no mrjob path configured"}
		}

		argv := append([]string{a.Mrjob, a.Stagecode}, a.SrcArgs...)

		return runWrappedAdapter(ctx, meta, files, journal, append(argv, phase), phase)
	case ir.LangExec:
		argv := append([]string{a.Stagecode}, a.SrcArgs...)

		return runWrappedAdapter(ctx, meta, files, journal, append(argv, phase), phase)
	default:
		return &apperror.UnsupportedError{Construct: "adapter " + string(a.Lang)}
	}
}

// runPyAdapter runs a python stage directly. The adapter expects fd 3 to be its
// _log file and fd 4 to be an error channel (normally supplied by mrjob); we
// provide both. The stage failed if anything was written to the error channel.
func runPyAdapter(ctx context.Context, meta, files, journal string, a Adapter, phase string) error {
	python := a.Python
	if python == "" {
		python = defaultPython
	}

	cmd := exec.CommandContext(ctx, python, a.Shell, a.Stagecode, phase, meta, files, journal)
	cmd.Dir = files

	aio, err := openAdapterIO(meta)
	if err != nil {
		return err
	}
	defer aio.close()

	cmd.Stdout = aio.stdout
	cmd.Stderr = aio.stderr
	cmd.ExtraFiles = []*os.File{aio.logFile, aio.errW} // fd 3 = _log, fd 4 = errors

	return aio.run(cmd, phase, meta)
}

// runWrappedAdapter runs a comp (via mrjob) or exec stage, which manage the
// metadata protocol themselves. Failure is a non-empty _errors file or a
// non-zero exit.
func runWrappedAdapter(ctx context.Context, meta, files, journal string, argv []string, phase string) error {
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], meta, files, journal)...)
	cmd.Dir = files

	stdout, err := os.Create(filepath.Join(meta, "_stdout"))
	if err != nil {
		return fmt.Errorf("create _stdout: %w", err)
	}
	defer func() { _ = stdout.Close() }()

	stderr, err := os.Create(filepath.Join(meta, "_stderr"))
	if err != nil {
		return fmt.Errorf("create _stderr: %w", err)
	}
	defer func() { _ = stderr.Close() }()

	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()

	if data, err := os.ReadFile(filepath.Join(meta, "_errors")); err == nil && len(data) > 0 {
		return fmt.Errorf("%s phase: %w: %s", phase, errStageFailed, strings.TrimSpace(string(data)))
	}

	if runErr != nil {
		tail, _ := os.ReadFile(filepath.Join(meta, "_stderr"))

		return fmt.Errorf("adapter %s phase: %w: %s", phase, runErr, strings.TrimSpace(string(tail)))
	}

	return nil
}

// adapterIO holds the file descriptors the Martian adapter expects.
type adapterIO struct {
	logFile    *os.File
	stdout     *os.File
	stderr     *os.File
	errR, errW *os.File
}

func openAdapterIO(meta string) (*adapterIO, error) {
	aio := &adapterIO{}

	for _, f := range []struct {
		dst  **os.File
		name string
	}{{&aio.logFile, "_log"}, {&aio.stdout, "_stdout"}, {&aio.stderr, "_stderr"}} {
		file, err := os.Create(filepath.Join(meta, f.name))
		if err != nil {
			aio.close()

			return nil, fmt.Errorf("create %s: %w", f.name, err)
		}

		*f.dst = file
	}

	r, w, err := os.Pipe()
	if err != nil {
		aio.close()

		return nil, fmt.Errorf("create error pipe: %w", err)
	}

	aio.errR, aio.errW = r, w

	return aio, nil
}

// run starts the adapter, drains its error channel, and classifies the result.
func (aio *adapterIO) run(cmd *exec.Cmd, phase, meta string) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}

	// Close the parent's write end so reading the pipe ends when the child exits.
	_ = aio.errW.Close()
	aio.errW = nil

	stageErr, _ := io.ReadAll(aio.errR)
	waitErr := cmd.Wait()

	if msg := strings.TrimSpace(string(stageErr)); msg != "" {
		_ = os.WriteFile(filepath.Join(meta, "_errors"), stageErr, filePerm)

		return fmt.Errorf("%s phase: %w: %s", phase, errStageFailed, msg)
	}

	if waitErr != nil {
		tail, _ := os.ReadFile(filepath.Join(meta, "_stderr"))

		return fmt.Errorf("adapter %s phase: %w: %s", phase, waitErr, strings.TrimSpace(string(tail)))
	}

	return nil
}

func (aio *adapterIO) close() {
	for _, f := range []*os.File{aio.logFile, aio.stdout, aio.stderr, aio.errR, aio.errW} {
		if f != nil {
			_ = f.Close()
		}
	}
}
