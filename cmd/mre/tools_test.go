package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/eunmann/mro2nf/internal/job"
	vendormartian "github.com/eunmann/mro2nf/vendor-martian"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/syntax"
)

// mrjobBesideTest is the mrjob a task that names none runs: the one beside
// the running binary, here the test binary.
func mrjobBesideTest(t *testing.T) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	return filepath.Join(filepath.Dir(exe), "mrjob")
}

// embeddedAdapters reads each adapter mre embeds below python/ by name;
// vendor-martian's own test holds them to the vendored files.
func embeddedAdapters(t *testing.T) map[string]string {
	t.Helper()

	entries, err := vendormartian.Adapters.ReadDir("python")
	if err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}

	for _, e := range entries {
		raw, err := vendormartian.Adapters.ReadFile("python/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}

		files[e.Name()] = string(raw)
	}

	return files
}

// checkAdapters checks dir holds exactly the embedded adapters below
// python/, the shell executable, as mrjob executes it.
func checkAdapters(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(dir, "python"))
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}

	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, "python", e.Name()))
		if err != nil {
			t.Fatal(err)
		}

		got[e.Name()] = string(raw)
	}

	if diff := cmp.Diff(embeddedAdapters(t), got); diff != "" {
		t.Errorf("adapters in %s differ from the embedded ones (-want +got):\n%s", dir, diff)
	}

	info, err := os.Stat(filepath.Join(dir, "python", shellScript))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode()&0o111 == 0 {
		t.Errorf("%s mode %v is not executable", shellScript, info.Mode())
	}
}

// TestToolsDefaults checks a task that names no tools runs the mrjob beside
// mre and, for a py stage, the vendored adapters, written below its work dir.
func TestToolsDefaults(t *testing.T) {
	f := runFlags{rt: job.Runtime{Work: t.TempDir()}}
	if err := f.tools(job.Code{Type: syntax.PythonStage}); err != nil {
		t.Fatal(err)
	}

	want := job.Runtime{Work: f.rt.Work, Mrjob: mrjobBesideTest(t), Adapters: filepath.Join(f.rt.Work, adaptersDir)}
	if diff := cmp.Diff(want, f.rt); diff != "" {
		t.Errorf("runtime (-want +got):\n%s", diff)
	}

	checkAdapters(t, f.rt.Adapters)
}

// TestToolsFillsOnlyWhatTheTaskLeftOut checks a py task that names one tool
// keeps it and gets the default of the other, and that one naming its
// adapters gets none extracted.
func TestToolsFillsOnlyWhatTheTaskLeftOut(t *testing.T) {
	for name, tc := range map[string]struct {
		named     job.Runtime
		extracted bool
	}{
		"mrjob":    {job.Runtime{Mrjob: "/opt/mrjob"}, true},
		"adapters": {job.Runtime{Adapters: "/opt/adapters"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			tc.named.Work = work
			f := runFlags{rt: tc.named}

			if err := f.tools(job.Code{Type: syntax.PythonStage}); err != nil {
				t.Fatal(err)
			}

			want := job.Runtime{Work: work, Mrjob: tc.named.Mrjob, Adapters: tc.named.Adapters}
			if want.Mrjob == "" {
				want.Mrjob = mrjobBesideTest(t)
			}

			if want.Adapters == "" {
				want.Adapters = filepath.Join(work, adaptersDir)
			}

			if diff := cmp.Diff(want, f.rt); diff != "" {
				t.Errorf("runtime (-want +got):\n%s", diff)
			}

			if _, err := os.Stat(filepath.Join(work, adaptersDir)); os.IsNotExist(err) == tc.extracted {
				t.Errorf("adapters extracted = %v, want %v (stat: %v)", !os.IsNotExist(err), tc.extracted, err)
			}
		})
	}
}

// TestToolsAdaptersAbsolute checks the adapters a relative work dir holds
// are named by their absolute path, as mrjob runs the shell from the job's
// own dir.
func TestToolsAdaptersAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	f := runFlags{rt: job.Runtime{Work: "work", Mrjob: "/opt/mrjob"}}
	if err := f.tools(job.Code{Type: syntax.PythonStage}); err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(cwd, "work", adaptersDir); f.rt.Adapters != want {
		t.Errorf("Adapters = %q, want %q", f.rt.Adapters, want)
	}

	checkAdapters(t, f.rt.Adapters)
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

// TestExtractAdaptersReplacesAStaleCopy checks an extraction over adapters
// already in dir writes the embedded ones, so a job never runs another
// copy's.
func TestExtractAdaptersReplacesAStaleCopy(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "python"), job.DirPerm); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "python", "martian.py"), []byte("stale\n"), filePerm); err != nil {
		t.Fatal(err)
	}

	if err := extractAdapters(dir); err != nil {
		t.Fatal(err)
	}

	checkAdapters(t, dir)
}

// TestToolsFailsWhereAdaptersCannotBeWritten checks a py task whose work dir
// cannot hold the adapters fails, naming the extraction, instead of running
// a job without them.
func TestToolsFailsWhereAdaptersCannotBeWritten(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, filePerm); err != nil {
		t.Fatal(err)
	}

	f := runFlags{rt: job.Runtime{Work: file, Mrjob: "/opt/mrjob"}}

	err := f.tools(job.Code{Type: syntax.PythonStage})
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "extract adapters") {
		t.Fatalf("tools over a file work dir: %v, want the extraction's ENOTDIR", err)
	}

	if f.rt.Adapters != "" {
		t.Errorf("Adapters = %q after a failed extraction, want unset", f.rt.Adapters)
	}
}
