package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/fileio"
	"github.com/eunmann/mro2nf/internal/job"
	vendormartian "github.com/eunmann/mro2nf/vendor-martian"
	"github.com/martian-lang/martian/martian/syntax"
)

const (
	// adaptersDir is where under the work dir a py stage's job runs the
	// adapters mre embeds from.
	adaptersDir = "_mre_adapters"
	// shellScript is the adapter mrjob executes, so its copy is executable.
	shellScript = "martian_shell.py"
)

// tools completes the runtime's tools a task did not name: the mrjob
// beside mre's own binary, as mrp runs the mrjob beside its own
// (core/runtime.go:287 util.RelPath), and for a py stage Martian's adapters
// mre embeds, written below the work dir.
func (f *runFlags) tools(code job.Code) error {
	if f.rt.Mrjob == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate mrjob beside mre: %w", err)
		}

		f.rt.Mrjob = filepath.Join(filepath.Dir(exe), "mrjob")
	}

	if f.rt.Adapters != "" || code.Type != syntax.PythonStage {
		return nil
	}

	dir, err := filepath.Abs(filepath.Join(f.rt.Work, adaptersDir))
	if err != nil {
		return fmt.Errorf("resolve adapters dir: %w", err)
	}

	if err := extractAdapters(dir); err != nil {
		return err
	}

	f.rt.Adapters = dir

	return nil
}

// extractAdapters writes the adapters mre embeds into dir.
func extractAdapters(dir string) error {
	err := fs.WalkDir(vendormartian.Adapters, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		dst := filepath.Join(dir, filepath.FromSlash(name))
		if d.IsDir() {
			return os.MkdirAll(dst, fileio.DirPerm)
		}

		raw, err := vendormartian.Adapters.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}

		perm := os.FileMode(fileio.FilePerm)
		if path.Base(name) == shellScript {
			perm = fileio.ExecPerm
		}

		return os.WriteFile(dst, raw, perm)
	})
	if err != nil {
		return fmt.Errorf("extract adapters: %w", err)
	}

	return nil
}
