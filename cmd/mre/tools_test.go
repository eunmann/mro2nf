package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/job"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// TestToolsDefaults checks a task that names no tools runs the mrjob beside
// mre and, for a py stage, the vendored adapters, written below its work dir
// with the shell executable, as mrjob executes it.
func TestToolsDefaults(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	f := runFlags{rt: job.Runtime{Work: t.TempDir()}}
	if err := f.tools(job.Code{Type: syntax.PythonStage}); err != nil {
		t.Fatal(err)
	}

	want := job.Runtime{Work: f.rt.Work, Mrjob: filepath.Join(filepath.Dir(exe), "mrjob"), Adapters: filepath.Join(f.rt.Work, adaptersDir)}
	if diff := cmp.Diff(want, f.rt); diff != "" {
		t.Errorf("runtime (-want +got):\n%s", diff)
	}

	vendored := filepath.Join("..", "..", "vendor-martian", "python")
	for _, name := range []string{"martian.py", shellScript} {
		got, err := os.ReadFile(filepath.Join(f.rt.Adapters, "python", name))
		if err != nil {
			t.Fatal(err)
		}

		raw, err := os.ReadFile(filepath.Join(vendored, name))
		if err != nil {
			t.Fatal(err)
		}

		if string(got) != string(raw) {
			t.Errorf("extracted %s differs from %s", name, filepath.Join(vendored, name))
		}
	}

	info, err := os.Stat(filepath.Join(f.rt.Adapters, "python", shellScript))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode()&0o111 == 0 {
		t.Errorf("%s mode %v is not executable", shellScript, info.Mode())
	}
}

// TestToolsKeepsWhatTheTaskNames checks named tools are kept, and that a
// stage the adapters do not run gets none extracted.
func TestToolsKeepsWhatTheTaskNames(t *testing.T) {
	for name, tc := range map[string]struct {
		rt   job.Runtime
		code syntax.StageCodeType
	}{
		"named": {job.Runtime{Mrjob: "/opt/mrjob", Adapters: "/opt/adapters"}, syntax.PythonStage},
		"comp":  {job.Runtime{Mrjob: "/opt/mrjob"}, syntax.CompiledStage},
		"exec":  {job.Runtime{Mrjob: "/opt/mrjob"}, syntax.ExecStage},
	} {
		t.Run(name, func(t *testing.T) {
			tc.rt.Work = t.TempDir()
			f := runFlags{rt: tc.rt}

			if err := f.tools(job.Code{Type: tc.code}); err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.rt, f.rt); diff != "" {
				t.Errorf("runtime (-want +got):\n%s", diff)
			}

			if _, err := os.Stat(filepath.Join(tc.rt.Work, adaptersDir)); !os.IsNotExist(err) {
				t.Errorf("adapters extracted for a task that does not run them (stat: %v)", err)
			}
		})
	}
}
