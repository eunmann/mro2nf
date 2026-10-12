//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestEntryFileGlobCharName checks an entry file override whose name holds a
// character bash would expand fails the run at PLAN, before any stage task
// stages it, naming the input and the character (#276). Nextflow's stage-in
// script leaves the source path unquoted on 25.10 and 26.04, so `s[1].txt`
// beside `s1.txt` would otherwise stage s1.txt silently, and a brace would be
// expanded; file(x, glob: false) keeps only the head node from globbing.
func TestEntryFileGlobCharName(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	cases := []struct {
		name, fixture, stage string
		params               func(t *testing.T, dir string) any
		want                 string
	}{
		{"glob_beside_match", "entry_file", "N0_COUNT", func(t *testing.T, dir string) any {
			t.Helper()
			writeFileT(t, filepath.Join(dir, "s[1].txt"), "10\n")
			writeFileT(t, filepath.Join(dir, "s1.txt"), "999\n")

			return map[string]string{"reads": filepath.Join(dir, "s[1].txt")}
		}, "entry input 'reads': the path contains '['"},
		{"brace_in_array", "entry_filearr", "N0_COUNTARR", func(_ *testing.T, dir string) any {
			return map[string][]string{"reads": {filepath.Join(dir, "a.txt"), filepath.Join(dir, "t{a,b}.txt")}}
		}, "entry input 'reads': the path contains '{'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proj := transpile(t, tc.fixture)
			dir := t.TempDir()

			params, err := json.Marshal(tc.params(t, dir))
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}

			writeFileT(t, filepath.Join(proj, "params.json"), string(params))

			if err := runNextflowAllowingFailedAttempts(t, proj, "-params-file", "params.json"); err == nil {
				t.Fatal("an override naming a file Nextflow would mis-stage must fail the run")
			}

			runLog := readRunLog(t, proj)

			for _, want := range []string{tc.want, "Nextflow staging limitation, see docs/TRANSPILER.md section 8"} {
				if !strings.Contains(runLog, want) {
					t.Errorf("run log does not report %q:\n%s", want, runLog)
				}
			}

			if strings.Contains(runLog, tc.stage) {
				t.Errorf("%s ran; the override must be refused before any stage task stages it:\n%s", tc.stage, runLog)
			}
		})
	}
}

// TestEntryFileRemoteURL checks an http:// file override is refused at PLAN,
// before the driver hands it to file(), which would GET any URL, the cloud
// metadata endpoint included (#330). With the mro2nf_allow_remote_inputs
// opt-in the same override is fetched, staged into the task that reads it,
// and reaches the stage.
func TestEntryFileRemoteURL(t *testing.T) {
	t.Parallel()

	requireTools(t, "nextflow", "java")

	fixtureDir := filepath.Join(root, "testdata", "entry_file")

	content, err := os.ReadFile(filepath.Join(fixtureDir, "input", "override.txt"))
	if err != nil {
		t.Fatalf("read override input: %v", err)
	}

	t.Run("rejected", func(t *testing.T) {
		t.Parallel()

		proj, hits := remoteReadsProject(t, content)

		if err := runNextflowAllowingFailedAttempts(t, proj, "-params-file", "params.json"); err == nil {
			t.Fatal("an http:// file override must fail the run")
		}

		if want := "entry input 'reads': scheme 'http' is not allowed"; !strings.Contains(readRunLog(t, proj), want) {
			t.Errorf("run log does not report %q:\n%s", want, readRunLog(t, proj))
		}

		if n := hits.Load(); n != 0 {
			t.Errorf("the rejected URL was fetched %d time(s)", n)
		}
	})

	t.Run("opted_in", func(t *testing.T) {
		t.Parallel()

		proj, hits := remoteReadsProject(t, content)

		if err := runNextflow(t, proj, "-params-file", "params.json", "--mro2nf_allow_remote_inputs", "true"); err != nil {
			t.Fatal(err)
		}

		goldenJSON(t, "entry_file",
			filepath.Join(proj, "results", "pipeline_outs.json"),
			filepath.Join(fixtureDir, "expected", "ep_override_outs.json"))

		if hits.Load() == 0 {
			t.Error("the opted-in URL was never fetched")
		}
	})
}

// remoteReadsProject transpiles entry_file with a params.json overriding reads
// with the URL of a local http server that serves body, and returns the
// project dir and the server's request count.
func remoteReadsProject(t *testing.T, body []byte) (string, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	proj := transpile(t, "entry_file")
	writeFileT(t, filepath.Join(proj, "params.json"), `{"reads": "`+srv.URL+`/override.txt"}`)

	return proj, &hits
}

func readRunLog(t *testing.T, proj string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(proj, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}

	return string(raw)
}
