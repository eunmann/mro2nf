package shim

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// scanSizeCap bounds the per-output content scan. The embedded-scratch-path
	// smell lives in small text manifests (a JSON/CSV that names other files by
	// absolute path), never in the multi-GB binary outputs (BAM/H5) a real
	// pipeline also produces — so files above the cap are skipped and the check
	// stays cheap on the hot path.
	scanSizeCap = 1 << 20 // 1 MiB
	// sniffBytes is how much of a file's head is checked for a NUL byte to skip
	// binary outputs before scanning.
	sniffBytes = 512
	// refCap bounds the reported path substring so a pathological line can't
	// blow up the diagnostic.
	refCap = 240
	// scanBufInit is the scanner's initial line buffer; it grows up to scanSizeCap.
	scanBufInit = 64 * 1024
)

// ScratchRef is one embedded absolute scratch path found in a staged output.
type ScratchRef struct {
	File string // bundle-relative path of the offending output file (f/L0000…)
	Path string // the offending absolute-path substring, for the diagnostic
}

// ScanOutputForScratchRefs walks the staged file leaves under bundleDir/f and
// reports any small text output whose CONTENT embeds scratchPrefix — the
// ephemeral task work dir. Such a path resolves on a shared filesystem (mrp,
// local, docker-iso) but NOT on an object-store backend (AWS Batch + S3,
// HealthOmics), where the downstream task is a different container that never
// sees this task's scratch: the stage baked a non-portable absolute path into
// its output instead of emitting a declared file leaf the data plane can stage.
// Best-effort and bounded (small text files only); it returns findings for the
// caller to log loudly and never fails the run.
func ScanOutputForScratchRefs(bundleDir, scratchPrefix string) []ScratchRef {
	if scratchPrefix == "" {
		return nil
	}

	var out []ScratchRef

	filesDir := filepath.Join(bundleDir, bundleFiles)
	// Best-effort: a per-entry walk error (or a missing f/ dir for a stage with
	// no file outs) yields a nil DirEntry, which we skip and keep scanning.
	_ = filepath.WalkDir(filesDir, func(path string, d os.DirEntry, _ error) error {
		if d == nil || d.IsDir() {
			return nil
		}

		if ref, ok := scanFileForPrefix(path, scratchPrefix); ok {
			rel, rerr := filepath.Rel(bundleDir, path)
			if rerr != nil {
				rel = path
			}

			out = append(out, ScratchRef{File: rel, Path: ref})
		}

		return nil
	})

	return out
}

// scanFileForPrefix reports the first line of a small, text-ish file that
// contains prefix, returning the offending absolute-path substring.
func scanFileForPrefix(path, prefix string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 || info.Size() > scanSizeCap {
		return "", false
	}

	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, sniffBytes)
	n, _ := io.ReadFull(f, head)
	if bytes.IndexByte(head[:n], 0) >= 0 { // a NUL byte => treat as binary, skip
		return "", false
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBufInit), scanSizeCap)
	for sc.Scan() {
		if i := strings.Index(sc.Text(), prefix); i >= 0 {
			return pathToken(sc.Text()[i:]), true
		}
	}

	return "", false
}

// pathToken extracts the absolute-path substring starting at s: up to the first
// delimiter that cannot appear in a path we care about (quote, whitespace,
// comma, or JSON punctuation), capped so the diagnostic stays readable.
func pathToken(s string) string {
	end := strings.IndexAny(s, "\"' \t,:]}")
	if end < 0 {
		end = len(s)
	}

	if end > refCap {
		end = refCap
	}

	return s[:end]
}
