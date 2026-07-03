package emit

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// runnerAssets holds the native Python stage runner (#79): run_stage.py calls
// the stage's split/main/join directly — no martian_shell.py adapter, no mre
// stage-execution hop — and its sibling martian.py is the compat shim stage
// code resolves via `import martian` (run_stage.py's dir is sys.path[0]). The
// files are static, so they embed rather than being rendered.
//
//go:embed runner/*.py
var runnerAssets embed.FS

// runnerDir is the embedded (and project-relative under _assets/) runner dir.
const runnerDir = "runner"

// writeRunner copies the embedded Python runner into <out>/_assets/runner/.
func writeRunner(outDir string) error {
	dst := filepath.Join(outDir, assetsDir, runnerDir)

	entries, err := runnerAssets.ReadDir(runnerDir)
	if err != nil {
		return fmt.Errorf("read embedded runner: %w", err)
	}

	if err := os.MkdirAll(dst, dirPerm); err != nil {
		return fmt.Errorf("create runner dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		// embed.FS paths are always slash-separated, so use path.Join (not
		// filepath.Join, which would use a backslash on Windows).
		data, err := fs.ReadFile(runnerAssets, path.Join(runnerDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read embedded runner %s: %w", e.Name(), err)
		}

		if err := writeFile(filepath.Join(dst, e.Name()), data); err != nil {
			return err
		}
	}

	return nil
}
