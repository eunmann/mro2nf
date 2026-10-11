// Command mre is the Martian runtime executor. It runs a single phase (split,
// main, or join) of a Martian stage against the original Martian adapter, for
// use inside a generated Nextflow process.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// version is set via -ldflags at build time.
var version = "dev"

var (
	errUsage        = errors.New("usage: mre <split|main|join|plan|args|run|post|outs|bind|forkbind|merge|gate|publish-layout|entryargs|version> [flags]")
	errUnknownPhase = errors.New("unknown phase")
	errBadInput     = errors.New("invalid -inputs pair (want id=file)")
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mre:", err)
		os.Exit(exitCode(err))
	}
}

// exitCode maps a stage failure to a process exit code: an ASSERT-class failure
// uses a distinct code so the generated Nextflow terminates rather than retries.
func exitCode(err error) int {
	if errors.Is(err, shim.ErrStageAssert) || errors.Is(err, job.ErrAssert) {
		return shim.AssertExitCode
	}

	return 1
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	if args[0] == "version" {
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mre", version)

		return nil
	}

	cmd, ok := subcommands()[args[0]]
	if !ok {
		return fmt.Errorf("%w: %q", errUnknownPhase, args[0])
	}

	return cmd(ctx, args[1:])
}

func subcommands() map[string]func(context.Context, []string) error {
	return map[string]func(context.Context, []string) error{
		"split":          runSplit,
		"main":           runMain,
		"join":           runJoin,
		"plan":           runPlan,
		"args":           runArgs,
		"post":           runPost,
		"outs":           runOuts,
		"run":            runJob,
		"bind":           runBind,
		"forkbind":       runForkBind,
		"merge":          runMerge,
		"gate":           runGate,
		"publish-layout": runPublishLayout,
		"entryargs":      runEntryArgs,
	}
}

// commonFlags are the adapter, resource, and invocation flags shared by every
// phase subcommand.
type commonFlags struct {
	shell, stagecode, lang   string
	mrjob, special           string
	srcArgs                  repeatedFlag
	call, mro, work, outFile string
	threads, memGB, vmemGB   float64
	overrideThreads          float64
	overrideMemGB            float64
	monitor                  bool
	confine                  bool
	attempt                  int
	allocBytes               int64
}

// repeatedFlag collects every occurrence of a repeatable string flag, in order.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, " ") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)

	return nil
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.shell, "shell", "", "path to martian_shell.py")
	fs.StringVar(&c.stagecode, "stagecode", "", "path to the stage code")
	fs.StringVar(&c.lang, "lang", "py", "stage adapter language")
	fs.StringVar(&c.mrjob, "mrjob", "", "path to mrjob (for comp stages)")
	fs.Var(&c.srcArgs, "srcarg", "extra stage src argument (repeatable, comp/exec stages)")
	fs.StringVar(&c.call, "call", "", "top-level pipeline/stage call name")
	fs.StringVar(&c.mro, "mro", "", "source MRO filename (for _jobinfo)")
	fs.StringVar(&c.work, "work", ".", "work directory for metadata/files")
	fs.StringVar(&c.outFile, "o", "", "output file (default stdout)")
	fs.Float64Var(&c.threads, "threads", 1, "the stage's requested threads")
	fs.Float64Var(&c.memGB, "memgb", 1, "the stage's requested memory in GB (0: the 1 GB job default)")
	fs.Float64Var(&c.vmemGB, "vmemgb", 0, "the stage's requested virtual memory in GB (0: memory plus headroom)")
	fs.StringVar(&c.special, "special", "", "the stage's using(special), reported as __special unless a chunk or join sets its own")
	fs.Float64Var(&c.overrideThreads, "overridethreads", 0, "an mrp --overrides thread count for this phase, beating a chunk's or join's own request (0: none)")
	fs.Float64Var(&c.overrideMemGB, "overridememgb", 0, "an mrp --overrides memory for this phase in GB, beating a chunk's or join's own request (0: none)")
	fs.BoolVar(&c.monitor, "monitor", false, "enforce mrp --monitor memory limits: an RSS kill at memgb plus a prlimit vmem cap at vmemgb")
	fs.BoolVar(&c.confine, "confine", false, "fail on a stage output that resolves outside the task work dir (isolated container targets) instead of copying it")
	fs.IntVar(&c.attempt, "attempt", 1, "the Nextflow task attempt; a retry's -monitor limits follow -allocbytes")
	fs.Int64Var(&c.allocBytes, "allocbytes", 0, "the memory Nextflow allocated the task, in bytes")

	return c
}

func (c *commonFlags) adapter() shim.Adapter {
	return shim.Adapter{
		Lang:       ir.Lang(c.lang),
		Shell:      c.shell,
		Stagecode:  c.stagecode,
		SrcArgs:    c.srcArgs,
		Mrjob:      c.mrjob,
		Monitor:    c.monitor,
		Attempt:    c.attempt,
		AllocBytes: c.allocBytes,
	}
}

// confinement returns the Confinement for the outputs of phase run in the work
// dir, strict under -confine.
func (c *commonFlags) confinement(phase string) (*shim.Confinement, error) {
	conf, err := shim.ConfineStage(c.work, phase, c.confine)
	if err != nil {
		return nil, fmt.Errorf("confine %s outputs: %w", phase, err)
	}

	return conf, nil
}

// resources is the phase's request: the stage's using() with any override.
func (c *commonFlags) resources() shim.Resources {
	return c.overridden(shim.Resources{Threads: c.threads, MemGB: c.memGB, VMemGB: c.vmemGB, Special: c.special})
}

// overridden applies the phase's overrides over def, a chunk's or join's own
// request.
func (c *commonFlags) overridden(def shim.Resources) shim.Resources {
	return shim.ApplyOverride(def, shim.Resources{Threads: c.overrideThreads, MemGB: c.overrideMemGB})
}

// joinRequest is the join's own request: the split-returned joinres.json with
// the phase's override over it, since mrp's getJobReqs applies --overrides
// after the join def.
func (c *commonFlags) joinRequest(path string) (shim.Resources, error) {
	res, err := readJoinRes(path)
	if err != nil {
		return res, err
	}

	return c.overridden(res), nil
}

func (c *commonFlags) invocation(stage string, args json.RawMessage) shim.Invocation {
	return shim.Invocation{Stage: stage, Call: c.call, Args: args, MROFile: c.mro}
}

func runSplit(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	cf := addCommon(fs)
	prod := addProducer(fs, types.RoleChunkIn)
	argsDir := fs.String("args", "", "stage args bundle dir")
	chunkDir := fs.String("chunkdir", "", "if set, write per-chunk bundles (chunk_NNNNN/) here")
	joinResOut := fs.String("joinres", "", "if set, write the split's join-phase resource override here")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readBundle(*argsDir)
	if err != nil {
		return err
	}

	if stageArgs, err = prod.coerceInputs(stageArgs); err != nil {
		return err
	}

	defs, joinRes, err := shim.RunSplit(ctx, cf.work, cf.adapter(), stageArgs, cf.resources(), cf.invocation(prod.callable, stageArgs))
	if err != nil {
		return fmt.Errorf("split: %w", err)
	}

	// chunks.json is the defs summary the join phase consumes; the per-chunk
	// bundles carry each chunk's staged input files to the main phase.
	if err := writeJSON(cf.outFile, defs); err != nil {
		return err
	}

	// joinres.json carries the split-returned join-phase resource override so the
	// generated JOIN process provisions cpus/memory/special to match mrp.
	if *joinResOut != "" {
		if err := writeJSON(*joinResOut, joinRes); err != nil {
			return err
		}
	}

	if *chunkDir == "" {
		return nil
	}

	conf, err := cf.confinement("split")
	if err != nil {
		return err
	}

	return writeChunkBundles(*chunkDir, defs, prod, conf)
}

// writeChunkBundles writes each chunk definition as a zero-padded bundle dir so
// a lexical sort of the names recovers chunk order downstream. The files are the
// split's own, so conf confines them.
func writeChunkBundles(dir string, defs []shim.ChunkDef, prod *producer, conf *shim.Confinement) error {
	man, err := prod.manifest()
	if err != nil {
		return err
	}

	chunkIn, err := man.Params(prod.callable, types.RoleChunkIn)
	if err != nil {
		return fmt.Errorf("chunk params: %w", err)
	}

	tbl := man.Table()

	for i, def := range defs {
		name := fmt.Sprintf(chunkDefPrefix+"%05d", i)
		if err := shim.WriteChunkBundle(filepath.Join(dir, name), def, chunkIn, tbl, conf); err != nil {
			return fmt.Errorf("write chunk bundle %s: %w", name, err)
		}
	}

	return nil
}

func runMain(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("main", flag.ContinueOnError)
	cf := addCommon(fs)
	prod := addProducer(fs, types.RoleMainOut)
	argsDir := fs.String("args", "", "stage args bundle dir")
	chunkDir := fs.String("chunk", "", "chunk bundle dir")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readBundle(*argsDir)
	if err != nil {
		return err
	}

	if stageArgs, err = prod.coerceInputs(stageArgs); err != nil {
		return err
	}

	chunkRaw, err := readBundle(*chunkDir)
	if err != nil {
		return err
	}

	chunk, err := decodeChunk(chunkRaw)
	if err != nil {
		return err
	}

	chunk.Resources = cf.overridden(chunk.Resources)

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	params, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("main params: %w", err)
	}

	outs, err := shim.RunMain(ctx, cf.work, cf.adapter(), stageArgs, chunk,
		params, man.Table(), cf.resources(), cf.invocation(prod.callable, stageArgs))
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	if *chunkDir == "" {
		if err := prod.validateOuts(outs); err != nil {
			return fmt.Errorf("main: %w", err)
		}
	}

	if prod.confine, err = cf.confinement("main"); err != nil {
		return err
	}

	if err := prod.write(cf.outFile, outs); err != nil {
		return err
	}

	warnOutputScratchRefs(cf.outFile, cf.work)

	return nil
}

func runJoin(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	cf := addCommon(fs)
	prod := addProducer(fs, types.RoleOut)
	argsDir := fs.String("args", "", "stage args bundle dir")
	chunksFile := fs.String("chunksfile", "", "the split's chunks.json, whose chunk count the gather checks against")
	chunkDir := fs.String("chunkdir", ".", "directory holding the staged "+chunkDefPrefix+"N def and "+chunkOutPrefix+"N output bundles")
	joinResFile := fs.String("joinres", "", "the split's joinres.json, the join phase's resource override")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readBundle(*argsDir)
	if err != nil {
		return err
	}

	if stageArgs, err = prod.coerceInputs(stageArgs); err != nil {
		return err
	}

	defs, outs, err := readChunkData(*chunksFile, *chunkDir)
	if err != nil {
		return err
	}

	joinRes, err := cf.joinRequest(*joinResFile)
	if err != nil {
		return err
	}

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	params, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("join params: %w", err)
	}

	final, err := shim.RunJoin(ctx, cf.work, cf.adapter(), stageArgs, defs, outs,
		params, man.Table(), joinRes, cf.resources(), cf.invocation(prod.callable, stageArgs))
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}

	if err := prod.validateOuts(final); err != nil {
		return fmt.Errorf("join: %w", err)
	}

	if prod.confine, err = cf.confinement("join"); err != nil {
		return err
	}

	if err := prod.write(cf.outFile, final); err != nil {
		return err
	}

	warnOutputScratchRefs(cf.outFile, cf.work)

	return nil
}
