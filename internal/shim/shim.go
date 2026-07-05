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
	"sync"
	"syscall"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
)

const (
	defaultPython = "python3"
	// extraVMemGB matches jobmanagers/config.json extra_vmem_per_job: the
	// default virtual-memory headroom added on top of the memory request.
	extraVMemGB = 3
	filePerm    = 0o644
	dirPerm     = 0o755
	disableFlag = "disable"
	// assertPrefix marks a non-retryable assertion failure on the adapter error
	// channel (mrp's write_assert prepends it).
	assertPrefix = "ASSERT:"
	// AssertExitCode is the process exit code the shim uses for an ASSERT-class
	// failure, letting the generated Nextflow terminate rather than retry it.
	AssertExitCode = 42
	bytesPerGB     = 1 << 30
)

var (
	// errStageFailed indicates the stage reported a (retryable) error via the
	// adapter's error channel (fd 4).
	errStageFailed = errors.New("stage failed")
	// ErrStageAssert is a non-retryable assertion failure (mrp ASSERT:).
	ErrStageAssert = errors.New("stage assertion failed")
)

// stageFailure builds the error for a stage that reported msg on its error
// channel, classifying an ASSERT (non-retryable) distinctly from an ordinary
// (retryable) failure.
func stageFailure(phase, msg string) error {
	sentinel := errStageFailed
	if strings.HasPrefix(msg, assertPrefix) {
		sentinel = ErrStageAssert
	}

	return fmt.Errorf("%s phase: %w: %s", phase, sentinel, msg)
}

// Adapter locates the stage code and the Martian adapter that runs it.
type Adapter struct {
	// Lang is the stage adapter language (only py is supported so far).
	Lang ir.Lang
	// Shell is the path to martian_shell.py.
	Shell string
	// Stagecode is the path to the stage module or binary.
	Stagecode string
	// SrcArgs are extra args from the stage's src declaration (`src exec
	// "code.py a b"`). Only comp/exec stages can carry them: the Martian
	// compiler rejects src args for py stages (syntax/compile_stages.go
	// "py stage type cannot have additional arguments").
	SrcArgs []string
	// Mrjob is the path to the mrjob wrapper used to run comp stages.
	Mrjob string
	// Monitor, when set, enforces the mrp --monitor limits: a hard vmem_gb cap on
	// address space via prlimit, plus a resident-memory (RSS) kill at mem_gb via
	// the process-group monitor (mrp's primary monitor kill).
	Monitor bool
}

// Resources is the per-phase resource allocation surfaced to stage code.
type Resources struct {
	Threads float64 `json:"threads"`
	MemGB   float64 `json:"mem_gb"`
	VMemGB  float64 `json:"vmem_gb"`
	Special string  `json:"special,omitempty"`
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

// RunSplit runs the split phase and returns the chunk definitions plus the
// optional join-phase resource override the split emitted (zero-valued when the
// split returned no `join` block).
func RunSplit(
	ctx context.Context, workDir string, a Adapter,
	stageArgs json.RawMessage, res Resources, inv Invocation,
) ([]ChunkDef, Resources, error) {
	meta, files, journal, err := prepDirs(workDir, "split")
	if err != nil {
		return nil, Resources{}, err
	}

	if err := writeRaw(filepath.Join(meta, "_args"), orEmptyObj(stageArgs)); err != nil {
		return nil, Resources{}, err
	}

	// Resolve once so the split phase reports the same magnitude as main/join: a
	// negative adaptive sentinel becomes |x| in _jobinfo (and the prlimit cap),
	// rather than leaking the raw negative to the split's get_memory_allocation().
	eff := resolveResources(Resources{}, res)
	if err := writeJobInfo(meta, files, "split", eff, inv); err != nil {
		return nil, Resources{}, err
	}

	if err := runAdapter(ctx, meta, files, journal, a, "split", eff); err != nil {
		return nil, Resources{}, err
	}

	return readStageDefs(meta)
}

// RunMain runs one chunk's main phase and returns that chunk's _outs.
func RunMain(
	ctx context.Context, workDir string, a Adapter, stageArgs json.RawMessage,
	chunk ChunkDef, outParams []ir.Param, tbl *types.Table, res Resources, inv Invocation,
) (json.RawMessage, error) {
	meta, files, journal, err := prepDirs(workDir, "main")
	if err != nil {
		return nil, err
	}

	args, err := mergeArgs(stageArgs, chunk, res)
	if err != nil {
		return nil, err
	}

	// _jobinfo must report the resolved per-chunk allocation (what the stage
	// actually got), not the raw phase request, matching mrp.
	eff := resolveResources(chunk.Resources, res)
	if err := stageInputs(meta, files, args, outParams, tbl, eff, inv, "main"); err != nil {
		return nil, err
	}

	if err := runAdapter(ctx, meta, files, journal, a, "main", eff); err != nil {
		return nil, err
	}

	return readRaw(filepath.Join(meta, "_outs"))
}

// RunJoin runs the join phase and returns the stage's final _outs. chunkDefs
// and chunkOuts must be in matching chunk order.
func RunJoin(
	ctx context.Context, workDir string, a Adapter, stageArgs json.RawMessage,
	chunkDefs []ChunkDef, chunkOuts []json.RawMessage,
	outParams []ir.Param, tbl *types.Table, res Resources, inv Invocation,
) (json.RawMessage, error) {
	meta, files, journal, err := prepDirs(workDir, "join")
	if err != nil {
		return nil, err
	}

	args, err := withResources(stageArgs, res)
	if err != nil {
		return nil, err
	}

	eff := resolveResources(Resources{}, res)
	if err := stageInputs(meta, files, args, outParams, tbl, eff, inv, "join"); err != nil {
		return nil, err
	}

	if err := writeChunkData(meta, chunkDefs, chunkOuts); err != nil {
		return nil, err
	}

	if err := runAdapter(ctx, meta, files, journal, a, "join", eff); err != nil {
		return nil, err
	}

	return readRaw(filepath.Join(meta, "_outs"))
}

// stageInputs writes the per-phase _args, _jobinfo, and skeleton _outs.
func stageInputs(
	meta, files string, args json.RawMessage,
	outParams []ir.Param, tbl *types.Table, res Resources, inv Invocation, phase string,
) error {
	if err := writeRaw(filepath.Join(meta, "_args"), args); err != nil {
		return err
	}

	if err := writeJobInfo(meta, files, phase, res, inv); err != nil {
		return err
	}

	return writeSkeletonOuts(meta, files, outParams, tbl)
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
// res is the resolved allocation: its vmem_gb caps address space (prlimit) and
// its mem_gb bounds resident memory (the RSS monitor) when monitoring is enabled.
func runAdapter(ctx context.Context, meta, files, journal string, a Adapter, phase string, res Resources) error {
	switch a.Lang {
	// py never sees SrcArgs: mrp's python arm builds a fixed argv with no args
	// slot (martian/core/node.go runChunk panics on py src args) and the
	// Martian compiler rejects them at parse time, so none can reach here.
	case ir.LangPy:
		return runPyAdapter(ctx, meta, files, journal, a, phase, res)
	// comp matches mrp: `mrjob <stagecode> <srcargs...> <phase> <meta> <files>
	// <journal>` (martian/core/node.go runChunk, CompiledStage arm).
	case ir.LangComp:
		if a.Mrjob == "" {
			return &apperror.UnsupportedError{Construct: "comp adapter", Detail: "no mrjob path configured"}
		}

		argv := append([]string{a.Mrjob, a.Stagecode}, a.SrcArgs...)

		return runWrappedAdapter(ctx, meta, files, journal, a, append(argv, phase), phase, res)
	// exec matches mrp: `<stagecode> <srcargs...> <phase> <meta> <files>
	// <journal>` (martian/core/node.go runChunk, ExecStage arm).
	case ir.LangExec:
		argv := append([]string{a.Stagecode}, a.SrcArgs...)

		return runWrappedAdapter(ctx, meta, files, journal, a, append(argv, phase), phase, res)
	default:
		return &apperror.UnsupportedError{Construct: "adapter " + string(a.Lang)}
	}
}

// startMonitor makes cmd a process-group leader and returns a resident-memory
// monitor for it, or nil when monitoring is off or no mem_gb limit is set. Call
// before cmd.Start; after Start, set the monitor's pgid and launch watch.
func startMonitor(cmd *exec.Cmd, a Adapter, memGB float64) *memMonitor {
	if !a.Monitor || memGB <= 0 {
		return nil
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setpgid = true

	return &memMonitor{limitBytes: int64(memGB * bytesPerGB)}
}

// limitedCommand builds the adapter command, capping its virtual memory via
// prlimit when monitoring is enabled and a vmem ceiling is set. The cap uses
// RLIMIT_AS (address space); a stage exceeding it fails its allocation. This is
// the hard address-space bound; the resident-memory (RSS-vs-mem_gb) kill mrp's
// monitor also applies is enforced separately by the process-group memMonitor.
// Absence of prlimit is tolerated (best effort).
func limitedCommand(ctx context.Context, a Adapter, vmemGB float64, name string, args ...string) *exec.Cmd {
	argv := append([]string{name}, args...)

	if a.Monitor && vmemGB > 0 {
		if path, err := exec.LookPath("prlimit"); err == nil {
			argv = append([]string{path, fmt.Sprintf("--as=%d", int64(vmemGB*bytesPerGB)), "--"}, argv...)
		} else {
			// Monitoring was requested but cannot be enforced; say so loudly
			// rather than silently running the stage with no memory ceiling.
			fmt.Fprintf(os.Stderr, "mre: --monitor requested but prlimit not found; running without a %g GB vmem cap\n", vmemGB)
		}
	}

	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// adapterEnv is the stage child's environment: the shim's own env with TMPDIR
// pointed at the phase's per-stage tmp dir (created by prepDirs), matching
// mrp's per-job TMPDIR so stage tempfiles land on the task's scratch volume
// rather than the container's (possibly tiny) /tmp.
func adapterEnv(meta string) []string {
	return append(os.Environ(), "TMPDIR="+filepath.Join(meta, "tmp"))
}

// runPyAdapter runs a python stage directly. The adapter expects fd 3 to be its
// _log file and fd 4 to be an error channel (normally supplied by mrjob); we
// provide both. The stage failed if anything was written to the error channel.
func runPyAdapter(ctx context.Context, meta, files, journal string, a Adapter, phase string, res Resources) error {
	cmd := limitedCommand(ctx, a, res.VMemGB, defaultPython, a.Shell, a.Stagecode, phase, meta, files, journal)
	cmd.Dir = files
	cmd.Env = adapterEnv(meta)
	mon := startMonitor(cmd, a, res.MemGB)

	aio, err := openAdapterIO(meta)
	if err != nil {
		return err
	}
	defer aio.close()

	// Tee the stage's stdout/stderr to the shim's own streams as well as the
	// _stdout/_stderr files, so they land in the task's captured logs
	// (.command.out/.err -> CloudWatch on Batch, GetRunTask on HealthOmics,
	// `nextflow log` locally). Without this the stage's output would only sit in
	// the per-task scratch and be lost on an object-store backend.
	cmd.Stdout = io.MultiWriter(aio.stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(aio.stderr, os.Stderr)
	cmd.ExtraFiles = []*os.File{aio.logFile, aio.errW} // fd 3 = _log, fd 4 = errors

	return aio.run(ctx, cmd, phase, meta, mon)
}

// runWrappedAdapter runs a comp (via mrjob) or exec stage, which manage the
// metadata protocol themselves. Failure is a non-empty _errors file or a
// non-zero exit.
func runWrappedAdapter(ctx context.Context, meta, files, journal string, a Adapter, argv []string, phase string, res Resources) error {
	cmd := limitedCommand(ctx, a, res.VMemGB, argv[0], append(argv[1:], meta, files, journal)...)
	cmd.Dir = files
	cmd.Env = adapterEnv(meta)
	mon := startMonitor(cmd, a, res.MemGB)

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

	cmd.Stdout = io.MultiWriter(stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}

	stop := beginMonitor(ctx, cmd, mon)
	runErr := cmd.Wait()

	stop() // join the monitor before reading its verdict; no sample in flight
	mon.recordExitPeak(cmd.ProcessState)
	forwardStageLog(meta)

	// mrjob routes a stage assertion to _assert (with the ASSERT: prefix stripped)
	// and exits 0, so it must be checked before _errors and the exit code or the
	// assertion is silently treated as success. Re-add the prefix so stageFailure
	// classifies it as the non-retryable ErrStageAssert. A stage-written _assert /
	// _errors is authoritative and is checked before the monitor verdict, so a
	// coincident memory kill cannot mask a non-retryable assertion.
	if data, err := os.ReadFile(filepath.Join(meta, "_assert")); err == nil && len(data) > 0 {
		return stageFailure(phase, assertPrefix+strings.TrimSpace(string(data)))
	}

	if data, err := os.ReadFile(filepath.Join(meta, "_errors")); err == nil && len(data) > 0 {
		return stageFailure(phase, strings.TrimSpace(string(data)))
	}

	if err := memViolation(mon, phase, meta); err != nil {
		return err
	}

	if runErr != nil {
		tail, _ := os.ReadFile(filepath.Join(meta, "_stderr"))

		return fmt.Errorf("adapter %s phase: %w: %s", phase, runErr, strings.TrimSpace(string(tail)))
	}

	return nil
}

// forwardStageLog surfaces a stage's Martian log (_log, written via
// martian.log_info on fd 3) to the shim's stderr, so it appears in the task's
// captured logs rather than only in the per-task scratch (which an object-store
// backend does not retain). Best-effort: a missing or empty log is silent.
func forwardStageLog(meta string) {
	data, err := os.ReadFile(filepath.Join(meta, "_log"))
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "--- martian stage log ---\n%s\n", strings.TrimRight(string(data), "\n"))
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
func (a *adapterIO) run(ctx context.Context, cmd *exec.Cmd, phase, meta string, mon *memMonitor) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}

	stop := beginMonitor(ctx, cmd, mon)
	defer stop()

	// Close the parent's write end so reading the pipe ends when the child exits.
	_ = a.errW.Close()
	a.errW = nil

	stageErr, readErr := io.ReadAll(a.errR)
	waitErr := cmd.Wait()
	stop() // join the monitor before reading its verdict; no sample in flight
	mon.recordExitPeak(cmd.ProcessState)
	forwardStageLog(meta)

	return classifyAdapterResult(phase, meta, stageErr, readErr, waitErr, mon)
}

// classifyAdapterResult turns a py adapter's exit evidence — the fd-4 error
// channel contents (and any failure reading them), the process exit, and the
// memory-monitor verdict — into the phase's result.
func classifyAdapterResult(phase, meta string, stageErr []byte, readErr, waitErr error, mon *memMonitor) error {
	// A stage-reported message (incl. an ASSERT on fd 4) is authoritative and is
	// classified first, so a coincident memory kill cannot mask a non-retryable
	// assertion as a retryable memory failure.
	if msg := strings.TrimSpace(string(stageErr)); msg != "" {
		// Mirror the error into _errors for parity with mrjob; a failure to do
		// so does not change the outcome (we already have the message).
		_ = os.WriteFile(filepath.Join(meta, "_errors"), stageErr, filePerm)

		return stageFailure(phase, msg)
	}

	// A failed error-channel read means the stage's verdict is unknown — a
	// failure message may have been lost — so the phase must not pass even on a
	// clean exit.
	if readErr != nil {
		return fmt.Errorf("adapter %s phase: read stage error channel (fd 4): %w", phase, readErr)
	}

	if err := memViolation(mon, phase, meta); err != nil {
		return err
	}

	if waitErr != nil {
		tail, _ := os.ReadFile(filepath.Join(meta, "_stderr"))

		return fmt.Errorf("adapter %s phase: %w: %s", phase, waitErr, strings.TrimSpace(string(tail)))
	}

	return nil
}

// beginMonitor launches the memory monitor against the started command's process
// group and returns a stop function; a nil monitor yields a no-op stop. The watch
// derives from the run's context so a cancelled run also stops the monitor. stop
// is idempotent and BLOCKS until the watch goroutine has exited, so the caller
// can read the monitor's violation flag knowing no sample is still in flight and
// no delayed kill can fire after the child is reaped.
func beginMonitor(ctx context.Context, cmd *exec.Cmd, mon *memMonitor) func() {
	if mon == nil {
		return func() {}
	}

	mon.pgid = cmd.Process.Pid
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		mon.watch(wctx)
	}()

	var once sync.Once

	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// memViolation returns the retryable failure for a monitor breach (writing the
// quota message to _errors like mrjob), or nil when no breach occurred. A memory
// kill is not an ASSERT, so it stays retryable — Nextflow retries with the
// escalated memory (memory * task.attempt).
func memViolation(mon *memMonitor, phase, meta string) error {
	if mon == nil || !mon.violated.Load() {
		return nil
	}

	msg := mon.message()
	_ = os.WriteFile(filepath.Join(meta, "_errors"), []byte(msg), filePerm)

	return stageFailure(phase, msg)
}

func (a *adapterIO) close() {
	for _, f := range []*os.File{a.logFile, a.stdout, a.stderr, a.errR, a.errW} {
		if f != nil {
			_ = f.Close()
		}
	}
}
