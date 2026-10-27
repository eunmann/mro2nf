// Package job runs one phase of a stage instance as mrp runs a job: it
// writes the metadata mrp writes before the job starts (core/stage.go), runs
// the stage code with mrp's argv, through mrjob for py and comp stages
// (core/node.go:1105 runJob), and reads the job's outcome from the metadata
// files it left, as mrp's state machine does (core/metadata.go:465).
package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
	"github.com/martian-lang/martian/martian/util"
)

// The phases of a stage job, mrp's shell names (core/stage.go:1637-1646).
const (
	PhaseSplit = "split"
	PhaseMain  = "main"
	PhaseJoin  = "join"
)

// AssertExitCode is the exit code mre gives an ASSERT-class failure: the
// generated config terminates on it rather than retry.
const AssertExitCode = 42

var (
	// ErrAssert is a stage assertion (an _assert file), which mrp never
	// retries (core/node.go:858 isErrorTransient).
	ErrAssert = errors.New("stage assertion failed")

	errFailed     = errors.New("stage failed")
	errIncomplete = errors.New("job exited without completing")
	errNotStage   = errors.New("not a stage")
	errChunks     = errors.New("chunk outs do not match the chunks")
)

// journalSafe encodes a fork id for a job name, as core's encodeJournalName
// does (core/stage.go:436).
func journalSafe(forkID string) string {
	return strings.NewReplacer(".", "%2E", "/", "%2F").Replace(forkID)
}

// Runtime is what every job of one run shares.
type Runtime struct {
	// Adapters is Martian's adapters dir, holding python/martian_shell.py.
	Adapters string
	// Mrjob is the mrjob binary py and comp stages run under.
	Mrjob string
	// Monitor is mrp's --monitor, which mrjob enforces from _jobinfo.
	Monitor bool
	// Profile is mrp's --profile, the profile mode of every job no
	// overrides key sets one for; empty is mrp's default, disable.
	Profile core.ProfileMode
	// Overrides is mrp's --overrides, over every job's resources and
	// profile; nil overrides none.
	Overrides *core.PipestanceOverrides
	// Local is the pool mrp's local job manager grants every job from; the
	// zero Pool grants them as mrp's cluster job managers do.
	Local Pool
	// PSID is the run's pipestance id, which names every job.
	PSID string
	// UUID is the run's pipestance uuid, which every job's environment
	// holds as MRO_UUID (core/pipestance.go:879 SetUuid).
	UUID string
	// Invocation and Version are recorded in every job's _jobinfo.
	Invocation *core.InvocationData
	Version    core.VersionInfo
	// Work is the dir that holds the jobs' metadata dirs and journal.
	Work string
}

// Versions is the version record mrp writes to every _jobinfo
// (core/pipestance.go:995 NewTopNode): the Martian runtime's embedded
// version and the pipelines' version the run found (PipelinesVersion).
func Versions(pipelines string) core.VersionInfo {
	return core.VersionInfo{Martian: util.GetVersion(), Pipelines: pipelines}
}

// PipelinesVersion is the pipelines' version found on mroPaths, as mrp
// finds it once, at its start (cmd/mrp/configure.go:252 GetMroVersion).
func PipelinesVersion(mroPaths []string) string {
	v, _ := util.GetMroVersion(mroPaths)

	return v
}

// Stage is a stage node as mrp's node runs it. Callable is the stage's
// declared name.
type Stage struct {
	Fqid      string
	Callable  string
	Code      Code
	Using     *core.JobResources
	Split     bool
	Outs      *syntax.OutParams
	ChunkOuts *syntax.OutParams
}

// Code is a stage's code: its type, the resolved command, and its src
// arguments.
type Code struct {
	Type syntax.StageCodeType
	Cmd  string
	Args []string
}

// NewStage is st as core.NewStagestance sets up a node
// (core/pipestance.go:35): its code found next to its .mro file, on
// mroPaths or on PATH (core/runtime.go:366), and its using() resources.
func NewStage(st *syntax.CallGraphStage, mroPaths []string) (Stage, error) {
	s, ok := st.Callable().(*syntax.Stage)
	if !ok {
		return Stage{}, fmt.Errorf("%w: %s", errNotStage, st.Fqid)
	}

	cmd, err := s.Src.FindPath(append(slices.Clone(mroPaths), filepath.SplitList(os.Getenv("PATH"))...))
	if err != nil {
		return Stage{}, fmt.Errorf("stage code of %s: %w", st.Fqid, err)
	}

	return Stage{
		Fqid:      st.Fqid,
		Callable:  st.Call().DecId,
		Code:      Code{Type: s.Src.Type, Cmd: cmd, Args: s.Src.Args},
		Using:     Using(s),
		Split:     s.Split,
		Outs:      s.OutParams,
		ChunkOuts: s.ChunkOuts,
	}, nil
}

// Using is s's using() resources, nil when it declares none.
func Using(s *syntax.Stage) *core.JobResources {
	r := s.Resources
	if r == nil {
		return nil
	}

	return &core.JobResources{
		Threads: float64(r.Threads), MemGB: float64(r.MemGB), VMemGB: float64(r.VMemGB), Special: r.Special,
	}
}

// Instance is one stage instance: its stage and mrp's id of its fork.
type Instance struct {
	Stage  Stage
	ForkID string
}

// Chunk is one chunk of an instance's main phase: its position among the
// split's chunks and its def.
type Chunk struct {
	Index, Count int
	Def          *core.ChunkDef
}

// Unsplit is the one chunk of a stage that does not split: an empty def
// (core/stage.go:419 NewFork).
func Unsplit() Chunk { return Chunk{Count: 1, Def: new(core.ChunkDef)} }

const chunkPrefix = "chnk"

// ChunkName is chunk i of n's name, its metadata dir and the last part of
// its job name (core/stage.go:98 NewChunk).
func ChunkName(i, n int) string { return fmt.Sprintf("%s%0*d", chunkPrefix, util.WidthForInt(n), i) }

// IsChunkName reports whether name is a ChunkName.
func IsChunkName(name string) bool {
	digits, ok := strings.CutPrefix(name, chunkPrefix)

	return ok && digits != "" && strings.Trim(digits, "0123456789") == ""
}

// top is the run's top node name (core/runtime.go:360).
func (r Runtime) top() string { return "ID." + r.PSID }

// fqname is mrp's name of node fqid, the top node's name before the call
// graph's fqid (core/runtime.go:360).
func (r Runtime) fqname(fqid string) string { return r.top() + "." + fqid }

// forkName is the instance's fork's job name (core/stage.go:438 updateId).
func (r Runtime) forkName(in Instance) string {
	return r.fqname(in.Stage.Fqid) + "." + journalSafe(in.ForkID)
}
