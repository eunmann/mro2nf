package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/eunmann/mro2nf/internal/cgnf"
	"github.com/eunmann/mro2nf/internal/emit"
)

// emitterV2 selects the call-graph emitter (internal/cgnf) while the
// integration branch carries both emitters (#463); the switch-over deletes
// the flag with the old emitter.
const emitterV2 = "v2"

var (
	errV2Flag     = errors.New("not supported by -emitter=v2")
	errV2NoShell  = errors.New("-emitter=v2 needs -shell, Martian's python adapter (<adapters>/python/martian_shell.py)")
	errBadEmitter = errors.New("unknown -emitter")
)

// hiddenEmitterFlag registers -emitter, which the usage text leaves out:
// it prints flag.PrintDefaults of a copy of fs without it.
func hiddenEmitterFlag(fs *flag.FlagSet) *string {
	v := fs.String("emitter", "", "")

	// Writing the usage text to the flag set's output cannot meaningfully
	// fail.
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage of %s:\n", fs.Name())

		shown := flag.NewFlagSet(fs.Name(), flag.ContinueOnError)
		shown.SetOutput(fs.Output())

		fs.VisitAll(func(f *flag.Flag) {
			if f.Name != "emitter" {
				shown.Var(f.Value, f.Name, f.Usage)
				shown.Lookup(f.Name).DefValue = f.DefValue
			}
		})

		shown.PrintDefaults()
	}

	return v
}

// emitV2 transpiles src with the call-graph emitter from the transpile
// options, refusing the old emitter's modes and file-copying.
func emitV2(src string, f transpileFlags, opts emit.Options) error {
	if opts.Native || opts.Container != "" || *f.target != "local" || opts.AllowExternalFiles || !opts.InlinePipelines {
		return fmt.Errorf("-native, -container, -target, -allow-external-files and -inline-pipelines=false: %w", errV2Flag)
	}

	if opts.Shell == "" {
		return errV2NoShell
	}

	if err := resolveToolPaths(&opts); err != nil {
		return err
	}

	mrjob := opts.Mrjob
	if mrjob == "" {
		mrjob = filepath.Join(filepath.Dir(opts.Mre), "mrjob")
	}

	paths, err := v2MroPaths(src, opts.MROPath)
	if err != nil {
		return err
	}

	cg := cgnf.Options{
		OutDir:   opts.OutDir,
		Mre:      opts.Mre,
		Mrjob:    mrjob,
		Adapters: filepath.Dir(filepath.Dir(opts.Shell)),
		MroPaths: paths,
		Monitor:  opts.Monitor,
		Confine:  opts.ConfineOutputs,
		Version:  opts.Version,
	}

	if err := cgnf.Emit(src, cg); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}

// v2MroPaths are the search path's roots, absolute, then the entry file's
// dir, where Martian resolves the entry's own includes and stage code.
func v2MroPaths(src string, mroPath []string) ([]string, error) {
	var out []string

	for _, p := range append(slices.Clone(mroPath), filepath.Dir(src)) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolve -mropath %s: %w", p, err)
		}

		if !slices.Contains(out, abs) {
			out = append(out, abs)
		}
	}

	return out, nil
}
