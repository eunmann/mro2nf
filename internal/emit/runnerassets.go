package emit

import (
	"embed"
	"path/filepath"
)

// runnerAssets holds the native Python stage runner (#79): run_stage.py calls
// the stage's split/main/join directly — no martian_shell.py adapter, no mre
// stage-execution hop — and its sibling martian.py is the compat shim stage
// code resolves via `import martian` (run_stage.py's dir is sys.path[0]). The
// files are static, so they embed rather than being rendered. The two files
// are named explicitly so the sibling test_runner.py unit tests never ship
// into generated projects.
//
//go:embed runner/run_stage.py runner/martian.py
var runnerAssets embed.FS

// runnerDir is the embedded (and project-relative under _assets/) runner dir.
const runnerDir = "runner"

// writeRunner copies the embedded Python runner into <out>/_assets/runner/.
func writeRunner(outDir string) error {
	return copyEmbeddedDir(runnerAssets, runnerDir, filepath.Join(outDir, assetsDir, runnerDir), "runner")
}
