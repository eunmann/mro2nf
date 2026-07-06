//go:build e2e

package e2e

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/google/go-cmp/cmp"
)

// leafMarkers are the transport-marker prefixes a stage's data.json may carry:
// a plain file (shim.FileMarker) or a staged directory (shim.DirMarker). The
// self-containment check must recognize both, or a directory-only bundle would
// silently pass its assertions.
var leafMarkers = []string{shim.FileMarker, shim.DirMarker}

// cloudConfig forces copy-staging into per-task scratch dirs: no input is
// reachable via a shared absolute path, only via Nextflow's staged copy of the
// channel item — the object-store execution model, simulated locally.
const cloudConfig = `process {
    scratch = true
    stageInMode = 'copy'
    stageOutMode = 'copy'
}
`

func writeCloudConfig(t *testing.T, proj string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(proj, "cloud.config"), []byte(cloudConfig), 0o644); err != nil {
		t.Fatalf("write cloud.config: %v", err)
	}
}

// TestCloudSimCopyStaging is the object-store readiness check (port of the
// file_chain half of cloud_sim.sh): the pipeline must still publish the right
// result under copy-staging, and each stage's output bundle must be
// self-contained — data.json references its files by a relative marker (never
// a host-absolute path) and physically contains them.
func TestCloudSimCopyStaging(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "file_chain")
	writeCloudConfig(t, proj)

	if err := runNextflow(t, proj, "-c", "cloud.config"); err != nil {
		t.Fatal(err)
	}

	var outs, want map[string]any

	readJSON(t, filepath.Join(proj, "results", "pipeline_outs.json"), &outs)

	// Decode the expected map the same way (UseNumber) so both sides are
	// json.Number — a float64 literal would never equal the json.Number got side.
	if err := decodeJSONNumber([]byte(`{"y": 42.0}`), &want); err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(want, outs); diff != "" {
		t.Errorf("pipeline outs mismatch under copy-staging (-want +got):\n%s", diff)
	}

	assertSelfContainedBundles(t, filepath.Join(proj, "work"))
}

// TestCloudSimMapFileMerge: a map call whose callee emits a FILE must carry
// per-fork files through the MERGE bundle, not bare absolute paths into
// deleted fork scratch dirs. (Port of the map_file half of cloud_sim.sh.)
func TestCloudSimMapFileMerge(t *testing.T) {
	requireTools(t, "nextflow", "java")

	proj := transpile(t, "map_file")
	writeCloudConfig(t, proj)

	if err := runNextflow(t, proj, "-c", "cloud.config"); err != nil {
		t.Fatal(err)
	}

	// The txt[] output `fs` publishes as an mrp-style tree: fs/<idx>.txt.
	assertFileContent(t, filepath.Join(proj, "results", "fs", "0.txt"), "val=1")
	assertFileContent(t, filepath.Join(proj, "results", "fs", "1.txt"), "val=2")
}

// assertSelfContainedBundles walks the work tree's data.json files and checks
// the bundle contract: every leaf marker (file or directory) is a relative
// bundle path (never absolute), the referenced leaf exists beside the
// data.json, and at least one such bundle exists at all (so the check cannot
// green-skip).
func assertSelfContainedBundles(t *testing.T, work string) {
	t.Helper()

	found := false

	walkErr := filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "data.json" {
			return err
		}

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		if !containsAnyMarker(string(raw)) {
			return nil
		}

		// The marker must be a relative bundle path, not an absolute one.
		for _, m := range leafMarkers {
			if strings.Contains(string(raw), `"`+m+`/`) {
				t.Errorf("absolute path in bundle marker (%s)", path)
			}
		}

		for _, rel := range bundleMarkers(raw) {
			if !fileExists(filepath.Join(filepath.Dir(path), rel)) {
				t.Errorf("bundle leaf %s missing beside %s", rel, path)

				continue
			}

			found = true
		}

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", work, walkErr)
	}

	if !found {
		t.Error("no self-contained leaf bundle found under work/")
	}
}

// containsAnyMarker reports whether s carries either transport-marker prefix.
func containsAnyMarker(s string) bool {
	for _, m := range leafMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}

	return false
}

// bundleMarkers extracts the relative paths of every top-level leaf marker
// (file or directory) in a data.json document (non-object documents carry no
// top-level markers).
func bundleMarkers(raw []byte) []string {
	var doc map[string]any

	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	var rels []string

	for _, v := range doc {
		if s, ok := v.(string); ok {
			if rel, _, isMarker := shim.CutMarker(s); isMarker {
				rels = append(rels, rel)
			}
		}
	}

	return rels
}
