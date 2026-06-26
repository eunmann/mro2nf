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

	"github.com/eunmann/martian-nextflow/internal/ir"
	"github.com/eunmann/martian-nextflow/internal/shim"
)

// version is set via -ldflags at build time.
var version = "dev"

var (
	errUsage        = errors.New("usage: mre <split|main|join|bind|version> [flags]")
	errUnknownPhase = errors.New("unknown phase")
	errBadInput     = errors.New("invalid -inputs pair (want id=file)")
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mre:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	switch phase := args[0]; phase {
	case "version":
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

	return c
}

func (c *commonFlags) adapter() shim.Adapter {
	return shim.Adapter{
		Lang:      ir.Lang(c.lang),
		Shell:     c.shell,
		Stagecode: c.stagecode,
		Mrjob:     c.mrjob,
	}
}

func (c *commonFlags) resources() shim.Resources {
	return shim.Resources{Threads: c.threads, MemGB: c.memGB, VMemGB: c.vmemGB}
}

func (c *commonFlags) invocation(args json.RawMessage) shim.Invocation {
	return shim.Invocation{Call: c.call, Args: args, MROFile: c.mro}
}

func (c *commonFlags) outNames() []string {
	if c.outs == "" {
		return nil
	}

	return splitComma(c.outs)
}

func runSplit(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	cf := addCommon(fs)
	argsFile := fs.String("args", "", "stage args JSON file")
	chunkDir := fs.String("chunkdir", "", "if set, also write per-chunk files (chunk_NNNNN.json) here")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readFile(*argsFile)
	if err != nil {
		return err
	}

	defs, err := shim.RunSplit(ctx, cf.work, cf.adapter(), stageArgs, cf.resources(), cf.invocation(stageArgs))
	if err != nil {
		return fmt.Errorf("split: %w", err)
	}

	if err := writeJSON(cf.outFile, defs); err != nil {
		return err
	}

	if *chunkDir != "" {
		return writeChunkFiles(*chunkDir, defs)
	}

	return nil
}

func runMain(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("main", flag.ContinueOnError)
	cf := addCommon(fs)
	argsFile := fs.String("args", "", "stage args JSON file")
	chunkFile := fs.String("chunk", "", "chunk definition JSON file")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readFile(*argsFile)
	if err != nil {
		return err
	}

	chunk, err := readChunk(*chunkFile)
	if err != nil {
		return err
	}

	outs, err := shim.RunMain(ctx, cf.work, cf.adapter(), stageArgs, chunk, cf.outNames(), cf.resources(), cf.invocation(stageArgs))
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	return writeRaw(cf.outFile, outs)
}

func runJoin(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	cf := addCommon(fs)
	argsFile := fs.String("args", "", "stage args JSON file")
	defsFile := fs.String("chunkdefs", "", "chunk defs JSON array file")
	outsFile := fs.String("chunkouts", "", "comma-separated per-chunk outs files, in order")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	stageArgs, err := readFile(*argsFile)
	if err != nil {
		return err
	}

	defs, outs, err := readChunkData(*defsFile, *outsFile)
	if err != nil {
		return err
	}

	final, err := shim.RunJoin(ctx, cf.work, cf.adapter(), stageArgs, defs, outs, cf.outNames(), cf.resources(), cf.invocation(stageArgs))
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}

	return writeRaw(cf.outFile, final)
}
