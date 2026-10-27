package e2e

// The stable digests of the Cell Ranger outputs mrp writes differently on
// each run. Each was measured against two mrp runs of the testrun
// invocation: what differs between them is masked, everything else is
// digested, so a file still compares with mro2nf's by all it holds that a
// run does not vary.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// crSamtools is the bundle's samtools, relative to its root.
const crSamtools = "lib/bin/samtools"

// errNoStableDigest reports a file mrp varies that no stable digest covers.
var errNoStableDigest = errors.New("no stable digest covers it")

// stableDigests maps a file extension to the digest of what mrp writes the
// same on each run.
var stableDigests = map[string]func(home, path string) (string, error){
	// filtered_feature_bc_matrix/*.gz: the gzip header's MTIME differs
	// between two mrp runs; the name and the decompressed bytes do not.
	".gz": gunzipDigest,
	// possorted_genome_bam.bam: the @PG line of the samtools cat that
	// merges it carries its command line, which names the join's dir in
	// the pipestance (on Nextflow, the task's work dir).
	".bam": bamDigest,
	// possorted_genome_bam.bam.bai: its virtual offsets move with the
	// BAM header's length; it is the index samtools index writes for the BAM.
	".bai": baiDigest,
	// cloupe.cloupe: the header's mode, the end of its gzipped index block
	// and the offset past it differ, as do the uuids the index holds; the
	// data blocks do not.
	".cloupe": cloupeDigest,
}

// stableDigest is the stable digest of path by its extension.
func stableDigest(home, path string) (string, error) {
	digest, ok := stableDigests[filepath.Ext(path)]
	if !ok {
		return "", fmt.Errorf("%s: %w", path, errNoStableDigest)
	}

	return digest(home, path)
}

// gunzipDigest digests a gzip file's member name and decompressed bytes.
func gunzipDigest(_, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("read gzip %s: %w", path, err)
	}

	h := sha256.New()
	h.Write([]byte(zr.Name + "\n"))

	if _, err := io.Copy(h, zr); err != nil {
		return "", fmt.Errorf("decompress %s: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// pgCommandLine matches the command-line field of a SAM @PG header line.
var pgCommandLine = regexp.MustCompile(`\tCL:[^\t]*`)

// bamDigest digests a BAM's header and records as the bundle's samtools
// prints them, less each @PG line's command line.
func bamDigest(home, path string) (string, error) {
	cmd := exec.Command(filepath.Join(home, crSamtools), "view", "--no-PG", "-h", path)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("samtools view: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("samtools view: %w", err)
	}

	h := sha256.New()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)

	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("@PG\t")) {
			line = pgCommandLine.ReplaceAll(line, nil)
		}

		h.Write(line)
		h.Write([]byte{'\n'})
	}

	if err := sc.Err(); err != nil {
		_ = cmd.Wait()

		return "", fmt.Errorf("read samtools view %s: %w", path, err)
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("samtools view %s: %w: %s", path, err, stderr.String())
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// baiDigest reports whether a BAM index is byte for byte the one the
// bundle's samtools index writes for the BAM beside it, or else the index's
// own digest.
func baiDigest(home, path string) (string, error) {
	dir, err := os.MkdirTemp("", "crbai")
	if err != nil {
		return "", fmt.Errorf("bai temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	want := filepath.Join(dir, "index.bai")

	cmd := exec.Command(filepath.Join(home, crSamtools), "index", "-o", want, strings.TrimSuffix(path, ".bai"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("samtools index %s: %w: %s", path, err, out)
	}

	a, err := os.ReadFile(want)
	if err != nil {
		return "", fmt.Errorf("read reindexed bai: %w", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read bai: %w", err)
	}

	if bytes.Equal(a, b) {
		return "samtools index of its BAM", nil
	}

	sum := sha256.Sum256(b)

	return "not samtools index of its BAM: " + hex.EncodeToString(sum[:]), nil
}

// cloupeHeader is the JSON header at the start of a cloupe file, less the
// fields mrp writes differently on each run (mode, nextHeaderOffset); the
// index block's End is read to find the block, then zeroed.
type cloupeHeader struct {
	Version     string `json:"version"`
	HeaderSize  int64  `json:"headerSize"`
	BlockOffset int64  `json:"blockOffset"`
	IndexBlock  struct {
		Start           int64 `json:"Start"`
		End             int64 `json:"End"`
		Kind            int64 `json:"Kind"`
		ArraySize       int64 `json:"ArraySize"`
		ArrayWidth      int64 `json:"ArrayWidth"`
		CompressionType int64 `json:"CompressionType"`
	} `json:"indexBlock"`
	Timestamp string `json:"timestamp"`
	P1        int64  `json:"p1"`
	P2        int64  `json:"p2"`
}

// errCloupeLayout reports a cloupe header whose blocks do not fit the file.
var errCloupeLayout = errors.New("cloupe blocks out of order or past the end")

// uuidText matches a textual uuid.
var uuidText = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// cloupeDigest digests a cloupe file's header less its varying fields, its
// data blocks, its decompressed index with each uuid masked, and anything
// after the index.
func cloupeDigest(_, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read cloupe: %w", err)
	}

	var hdr cloupeHeader
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&hdr); err != nil {
		return "", fmt.Errorf("decode cloupe header %s: %w", path, err)
	}

	start, end := hdr.IndexBlock.Start, hdr.IndexBlock.End
	if hdr.BlockOffset < 0 || hdr.BlockOffset > start || start > end || end > int64(len(raw)) {
		return "", fmt.Errorf("%s: %w", path, errCloupeLayout)
	}

	zr, err := gzip.NewReader(bytes.NewReader(raw[start:end]))
	if err != nil {
		return "", fmt.Errorf("read cloupe index %s: %w", path, err)
	}

	index, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("decompress cloupe index %s: %w", path, err)
	}

	hdr.IndexBlock.End = 0

	meta, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("encode cloupe header: %w", err)
	}

	h := sha256.New()
	for _, part := range [][]byte{meta, raw[hdr.BlockOffset:start], uuidText.ReplaceAll(index, []byte("<uuid>")), raw[end:]} {
		h.Write(part)
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
