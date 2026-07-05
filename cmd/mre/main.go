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

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
)

// version is set via -ldflags at build time.
var version = "dev"

var (
	errUsage        = errors.New("usage: mre <split|main|join|bind|forkbind|merge|publish-layout|entryargs|version> [flags]")
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
	if errors.Is(err, shim.ErrStageAssert) {
		return shim.AssertExitCode
	}

	return 1
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	switch phase := args[0]; phase {
	case "version":
		// Writing the version line to stdout cannot meaningfully fail.
		_, _ = fmt.Fprintln(os.Stdout, "mre", version)

		return nil
	case "split":
		return runSplit(ctx, args[1:])
	case "main":
		return runMain(ctx, args[1:])
	case "join":
		return runJoin(ctx, args[1:])
	case "bind":
		return runBind(ctx, args[1:])
	case "forkbind":
		return runForkBind(ctx, args[1:])
	case "merge":
		return runMerge(ctx, args[1:])
	case "publish-layout":
		return runPublishLayout(ctx, args[1:])
	case "entryargs":
		return runEntryArgs(ctx, args[1:])
	default:
		return fmt.Errorf("%w: %q", errUnknownPhase, phase)
	}
}

// commonFlags are the adapter, resource, and invocation flags shared by every
// phase subcommand.
type commonFlags struct {
	shell, stagecode, lang   string
	mrjob                    string
	call, mro, work, outFile string
	outs                     string
	threads, memGB, vmemGB   float64
	monitor                  bool
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.shell, "shell", "", "path to martian_shell.py")
	fs.StringVar(&c.stagecode, "stagecode", "", "path to the stage code")
	fs.StringVar(&c.lang, "lang", "py", "stage adapter language")
	fs.StringVar(&c.mrjob, "mrjob", "", "path to mrjob (for comp stages)")
	fs.StringVar(&c.call, "call", "", "top-level pipeline/stage call name")
	fs.StringVar(&c.mro, "mro", "", "source MRO filename (for _jobinfo)")
	fs.StringVar(&c.work, "work", ".", "work directory for metadata/files")
	fs.StringVar(&c.outFile, "o", "", "output file (default stdout)")
	fs.StringVar(&c.outs, "outs", "", "comma-separated output parameter names")
	fs.Float64Var(&c.threads, "threads", 1, "allocated threads")
	fs.Float64Var(&c.memGB, "memgb", 1, "allocated memory in GB")
	fs.Float64Var(&c.vmemGB, "vmemgb", 0, "allocated virtual memory in GB")
	fs.BoolVar(&c.monitor, "monitor", false, "cap stage virtual memory at vmem_gb via prlimit")

	return c
}

func (c *commonFlags) adapter() shim.Adapter {
	return shim.Adapter{
		Lang:      ir.Lang(c.lang),
		Shell:     c.shell,
		Stagecode: c.stagecode,
		Mrjob:     c.mrjob,
		Monitor:   c.monitor,
	}
}

func (c *commonFlags) resources() shim.Resources {
	return shim.Resources{Threads: c.threads, MemGB: c.memGB, VMemGB: c.vmemGB}
}

func (c *commonFlags) invocation(args json.RawMessage) shim.Invocation {
	return shim.Invocation{Call: c.call, Args: args, MROFile: c.mro}
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

	if stageArgs, err = prod.coerceInputs(stageArgs, types.RoleIn); err != nil {
		return err
	}

	defs, joinRes, err := shim.RunSplit(ctx, cf.work, cf.adapter(), stageArgs, cf.resources(), cf.invocation(stageArgs))
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

	if *chunkDir != "" {
		return writeChunkBundles(*chunkDir, defs, prod)
	}

	return nil
}

// writeChunkBundles writes each chunk definition as a zero-padded bundle dir so
// a lexical sort of the names recovers chunk order downstream.
func writeChunkBundles(dir string, defs []shim.ChunkDef, prod *producer) error {
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
		name := fmt.Sprintf("chunk_%05d", i)
		if err := shim.WriteChunkBundle(filepath.Join(dir, name), def, chunkIn, tbl); err != nil {
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

	if stageArgs, err = prod.coerceInputs(stageArgs, types.RoleIn); err != nil {
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

	if chunk, err = prod.coerceChunk(chunk); err != nil {
		return err
	}

	man, err := prod.manifest()
	if err != nil {
		return err
	}

	params, err := man.Params(prod.callable, prod.role)
	if err != nil {
		return fmt.Errorf("main params: %w", err)
	}

	outs, err := shim.RunMain(ctx, cf.work, cf.adapter(), stageArgs, chunk,
		params, man.Table(), cf.resources(), cf.invocation(stageArgs))
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	return prod.write(cf.outFile, outs)
}

func runJoin(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	cf := addCommon(fs)
	prod := addProducer(fs, types.RoleOut)
	argsDir := fs.String("args", "", "stage args bundle dir")
	defsFile := fs.String("chunkdefs", "", "chunk defs JSON array file")
	outsList := fs.String("chunkouts", "", "comma-separated per-chunk output bundle dirs, in order")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readBundle(*argsDir)
	if err != nil {
		return err
	}

	if stageArgs, err = prod.coerceInputs(stageArgs, types.RoleIn); err != nil {
		return err
	}

	defs, outs, err := readChunkData(*defsFile, *outsList)
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
		params, man.Table(), cf.resources(), cf.invocation(stageArgs))
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}

	return prod.write(cf.outFile, final)
}
