package emit

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// libAssets holds the hand-written Groovy helper library shipped verbatim into
// every generated project's lib/. Nextflow auto-adds lib/ (next to main.nf) to
// the driver classpath, so the classes are available to the generated workflow
// code without any include. The files are static — no generated identifiers —
// so they embed rather than being rendered.
//
//go:embed lib/*.groovy
var libAssets embed.FS

// libDir is the project-relative directory Nextflow scans for classpath classes.
const libDir = "lib"

// writeLib copies the embedded Groovy helper library into <out>/lib/. It rides
// along in the HealthOmics packaging zip (package.sh zips the project, excluding
// only runtime/) exactly like _assets/ and nulls/.
func writeLib(outDir string) error {
	return copyEmbeddedDir(libAssets, libDir, filepath.Join(outDir, libDir), "lib")
}

// copyEmbeddedDir writes every file of an embedded directory into dst, so the
// static asset trees (lib/, _assets/runner/) share one copy loop and staging
// fixes land in both.
func copyEmbeddedDir(fsys embed.FS, srcDir, dst, what string) error {
	entries, err := fsys.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read embedded %s: %w", what, err)
	}

	if err := os.MkdirAll(dst, dirPerm); err != nil {
		return fmt.Errorf("create %s dir: %w", what, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		// embed.FS paths are always slash-separated, so use path.Join (not
		// filepath.Join, which would use a backslash on Windows).
		data, err := fs.ReadFile(fsys, path.Join(srcDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read embedded %s %s: %w", what, e.Name(), err)
		}

		if err := writeFile(filepath.Join(dst, e.Name()), data); err != nil {
			return err
		}
	}

	return nil
}
