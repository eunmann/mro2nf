package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/eunmann/mro2nf/internal/cgnf"
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

// emitV2 transpiles src with the call-graph emitter, refusing the old
// emitter's modes and file-copying.
func emitV2(src string, f transpileFlags) error {
	if *f.native || *f.container != "" || *f.target != "local" || *f.allowExternalFiles || !*f.inlinePipelines {
		return fmt.Errorf("-native, -container, -target, -allow-external-files and -inline-pipelines=false: %w", errV2Flag)
	}

	if *f.shell == "" {
		return errV2NoShell
	}

	mre := absOrSelf(*f.mre)

	mrjob := absOrSelf(*f.mrjob)
	if mrjob == "" {
		mrjob = filepath.Join(filepath.Dir(mre), "mrjob")
	}

	paths, err := v2MroPaths(src, *f.mroPath)
	if err != nil {
		return err
	}

	opts := cgnf.Options{
		OutDir:   *f.outDir,
		Mre:      mre,
		Mrjob:    mrjob,
		Adapters: filepath.Dir(filepath.Dir(absOrSelf(*f.shell))),
		MroPaths: paths,
		Monitor:  *f.monitor,
		Confine:  *f.confineOutputs,
		Version:  buildVersion(),
	}

	if err := cgnf.Emit(src, opts); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	return nil
}

// v2MroPaths are the -mropath roots, absolute, then the entry file's dir,
// where Martian resolves the entry's own includes and stage code.
func v2MroPaths(src, mroPath string) ([]string, error) {
	var out []string

	for _, p := range append(filepath.SplitList(mroPath), filepath.Dir(src)) {
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
