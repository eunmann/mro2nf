package job

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
	"github.com/martian-lang/martian/martian/util"
)

// DirPerm is the mode of a job's metadata, files and tmp dirs.
const DirPerm = 0o755

const (
	disable  = "disable"
	monitor  = "monitor"
	jobMode  = "local"
	tempDir  = "tmp"
	filesDir = "files"
	// orphanStdioGrace is how long a job waits, once its process has exited,
	// for its stdout and stderr to close: mre tees them through pipes, which
	// a background child the stage left running can hold open.
	orphanStdioGrace = 2 * time.Second
	maxAlarmLine     = 1 << 20
)

// threadEnvs are jobmanagers/config.json's thread_envs, which mrp sets to a
// job's threads (core/jobmanager.go:65 threadEnvs).
func threadEnvs() []string {
	return []string{
		"GOMAXPROCS", "MKL_NUM_THREADS", "NUMEXPR_MAX_THREADS", "NUMEXPR_NUM_THREADS",
		"OMP_NUM_THREADS", "OMP_THREAD_LIMIT", "POLARS_MAX_THREADS", "ROOT_MAX_THREADS",
		"TF_NUM_THREADS",
	}
}

// job is one phase job: mrp's shell name for it, its name, its metadata
// and its resolved resources.
type job struct {
	shell   string
	name    string
	dir     string
	journal string
	meta    *core.Metadata
	res     core.JobResources
}

// newJob creates the metadata dir sub of the instance's job for shell,
// named name, with its files and tmp dirs and the journal dir.
func (r Runtime) newJob(shell, name, sub string) (*job, error) {
	work, err := filepath.Abs(r.Work)
	if err != nil {
		return nil, fmt.Errorf("resolve work dir: %w", err)
	}

	dir := filepath.Join(work, sub)
	journal := filepath.Join(work, "journal")

	for _, d := range []string{filepath.Join(dir, filesDir), filepath.Join(dir, tempDir), journal} {
		if err := os.MkdirAll(d, DirPerm); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	return &job{
		shell:   shell,
		name:    name,
		dir:     dir,
		journal: filepath.Join(journal, strings.TrimPrefix(name, r.top()+".")),
		meta:    core.NewMetadata(name, dir),
	}, nil
}

func (j *job) files() string { return j.meta.FilesPath() }

func (j *job) write(name core.MetadataFileName, v any) error {
	if err := j.meta.Write(name, v); err != nil {
		return fmt.Errorf("write _%s of %s: %w", name, j.name, err)
	}

	return nil
}

func (j *job) read(name core.MetadataFileName) ([]byte, bool, error) {
	raw, err := os.ReadFile(j.meta.MetadataFilePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read _%s of %s: %w", name, j.name, err)
	}

	return raw, true, nil
}

// argv is the command mrp runs for the job (core/node.go:1130-1160): py
// and comp stages under mrjob, an exec stage directly, each followed by the
// phase, the metadata and files dirs and the journal prefix.
func (r Runtime) argv(code Code, j *job) (string, []string) {
	tail := []string{j.shell, j.dir, j.files(), j.journal}

	switch code.Type {
	case syntax.PythonStage:
		return r.Mrjob, append([]string{filepath.Join(r.Adapters, "python", "martian_shell.py"), code.Cmd}, tail...)
	case syntax.CompiledStage:
		return r.Mrjob, append(append([]string{code.Cmd}, code.Args...), tail...)
	default:
		return code.Cmd, append(slices.Clone(code.Args), tail...)
	}
}

// jobInfo is the _jobinfo mrp writes before the job runs
// (core/node.go:1178-1190): every key the adapters read, with mrp's
// default profiling and stack-variable modes.
func (r Runtime) jobInfo(j *job) *core.JobInfo {
	mon := disable
	if r.Monitor {
		mon = monitor
	}

	version := r.Version

	return &core.JobInfo{
		Name:        j.name,
		Type:        jobMode,
		Threads:     j.res.Threads,
		MemGB:       j.res.MemGB,
		VMemGB:      j.res.VMemGB,
		ProfileMode: core.DisableProfile,
		Stackvars:   disable,
		Monitor:     mon,
		Invocation:  r.Invocation,
		Version:     &version,
	}
}

// env is the job's environment (core/jobmanager_local.go:515): mre's own,
// each thread variable set to the job's whole threads, and TMPDIR the
// job's tmp dir (core/node.go:1125).
func (j *job) env() []string {
	envs := map[string]string{}

	n := strconv.Itoa(int(math.Ceil(j.res.Threads)))
	for _, k := range threadEnvs() {
		envs[k] = n
	}

	envs["TMPDIR"] = filepath.Join(j.dir, tempDir)

	return util.MergeEnv(envs)
}

// run writes the job's _jobinfo, runs it, and returns its outcome.
func (r Runtime) run(ctx context.Context, code Code, j *job) error {
	if err := j.write(core.JobInfoFile, r.jobInfo(j)); err != nil {
		return err
	}

	name, argv := r.argv(code, j)
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = j.files()
	cmd.Env = j.env()
	cmd.SysProcAttr = util.Pdeathsig(&syscall.SysProcAttr{}, syscall.SIGTERM)
	cmd.WaitDelay = orphanStdioGrace

	closeStdio, err := j.stdio(cmd)
	if err != nil {
		return err
	}

	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) {
		fmt.Fprintf(os.Stderr, "mre: %s left a background process holding its stdout/stderr; "+
			"output it writes from now on is not captured\n", j.name)

		runErr = nil
	}

	if err := closeStdio(); err != nil {
		return err
	}

	j.forwardAlarms()

	return j.outcome(runErr)
}

// stdio gives the job mrp's _stdout and _stderr, with their headers
// (core/jobmanager_local.go:696 executeLocal), each also teed to mre's own
// stream so it reaches the task log.
func (j *job) stdio(cmd *exec.Cmd) (func() error, error) {
	var files []*os.File

	closeAll := func() error {
		errs := make([]error, 0, len(files))
		for _, f := range files {
			errs = append(errs, f.Close())
		}

		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("close stdio of %s: %w", j.name, err)
		}

		return nil
	}

	for _, s := range []struct {
		name core.MetadataFileName
		tee  io.Writer
		dst  *io.Writer
	}{{core.StdOut, os.Stdout, &cmd.Stdout}, {core.StdErr, os.Stderr, &cmd.Stderr}} {
		f, err := os.Create(j.meta.MetadataFilePath(s.name))
		if err != nil {
			_ = closeAll()

			return nil, fmt.Errorf("create _%s of %s: %w", s.name, j.name, err)
		}

		files = append(files, f)

		if _, err := fmt.Fprintf(f, "[%s]\n", s.name); err != nil {
			_ = closeAll()

			return nil, fmt.Errorf("write _%s of %s: %w", s.name, j.name, err)
		}

		*s.dst = io.MultiWriter(f, s.tee)
	}

	return closeAll, nil
}

// outcome reads the job's state as mrp does (core/metadata.go:465
// _getStateNoLock): an assertion, which mrp never retries
// (core/node.go:858 isErrorTransient), then an error, transient when mrp
// retries it, then completion. A job that exited non-zero without an error
// gets one, as mrp's local job manager writes it
// (core/jobmanager_local.go:667-672): "signal: killed" for a killed job.
func (j *job) outcome(runErr error) error {
	raw, ok, err := j.read(core.Assert)
	if err != nil {
		return err
	}

	if ok {
		return fmt.Errorf("%s in %s: %w: %s", j.name, j.dir, ErrAssert, strings.TrimSpace(string(raw)))
	}

	raw, ok, err = j.read(core.Errors)
	if err != nil {
		return err
	}

	if !ok && runErr != nil {
		if err := j.meta.WriteRaw(core.Errors, runErr.Error()); err != nil {
			return fmt.Errorf("write _errors of %s: %w", j.name, err)
		}

		raw, ok = []byte(runErr.Error()), true
	}

	if ok {
		failed := errFailed
		if transient(string(raw)) {
			failed = fmt.Errorf("%w (%w)", errFailed, ErrTransient)
		}

		return fmt.Errorf("%s in %s: %w: %s", j.name, j.dir, failed, strings.TrimSpace(string(raw)))
	}

	if _, ok, err := j.read(core.CompleteFile); err != nil || !ok {
		return errors.Join(err, fmt.Errorf("%s in %s: %w", j.name, j.dir, errIncomplete))
	}

	return nil
}

// forwardAlarms writes each line of the job's _alarm to mre's stderr, so
// the task log carries the alarms mrp prints when the run ends.
func (j *job) forwardAlarms() {
	f, err := os.Open(j.meta.MetadataFilePath(core.AlarmFile))
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(nil, maxAlarmLine)

	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			fmt.Fprintf(os.Stderr, "mre: ALARM: %s: %s\n", j.name, line)
		}
	}
}
