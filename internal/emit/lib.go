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
	entries, err := libAssets.ReadDir(libDir)
	if err != nil {
		return fmt.Errorf("read embedded lib: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(outDir, libDir), dirPerm); err != nil {
		return fmt.Errorf("create lib dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		// embed.FS paths are always slash-separated, so use path.Join (not
		// filepath.Join, which would use a backslash on Windows).
		data, err := fs.ReadFile(libAssets, path.Join(libDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read embedded lib %s: %w", e.Name(), err)
		}

		if err := writeFile(filepath.Join(outDir, libDir, e.Name()), data); err != nil {
			return err
		}
	}

	return nil
}
