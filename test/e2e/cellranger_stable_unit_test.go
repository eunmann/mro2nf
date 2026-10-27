package e2e

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeGzip writes data as a gzip member named name with the given mtime.
func writeGzip(t *testing.T, path, name string, mtime time.Time, data string) {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	zw.Name, zw.ModTime = name, mtime

	if _, err := zw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGunzipDigestIgnoresMtime checks a gzip file's digest ignores the
// header MTIME two mrp runs differ in, and keeps its name and contents.
func TestGunzipDigestIgnoresMtime(t *testing.T) {
	dir := t.TempDir()
	at := func(i int) time.Time { return time.Unix(int64(1_700_000_000+i), 0) }

	files := map[string]struct {
		name, data string
		mtime      time.Time
	}{
		"base.gz":    {"barcodes.tsv", "AAAC-1\n", at(0)},
		"mtime.gz":   {"barcodes.tsv", "AAAC-1\n", at(90)},
		"content.gz": {"barcodes.tsv", "AAAG-1\n", at(0)},
		"name.gz":    {"features.tsv", "AAAC-1\n", at(0)},
	}

	digests := map[string]string{}

	for file, f := range files {
		path := filepath.Join(dir, file)
		writeGzip(t, path, f.name, f.mtime, f.data)

		d, err := stableDigest("", path)
		if err != nil {
			t.Fatal(err)
		}

		digests[file] = d
	}

	if digests["base.gz"] != digests["mtime.gz"] {
		t.Error("the digest varies with the gzip MTIME")
	}

	for _, file := range []string{"content.gz", "name.gz"} {
		if digests[file] == digests["base.gz"] {
			t.Errorf("the digest misses the difference of %s", file)
		}
	}
}

// cloupeFile builds a cloupe file: a JSON header padded to 4096 bytes, the
// data blocks, then the gzipped index, which names indexUUID.
func cloupeFile(t *testing.T, data, indexUUID string, mode int64) []byte {
	t.Helper()

	var index bytes.Buffer

	zw := gzip.NewWriter(&index)
	if _, err := fmt.Fprintf(zw, `{"Uuid":%q,"FeatureCount":275}`, indexUUID); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	start := 4096 + len(data)
	end := start + index.Len()
	header := fmt.Sprintf(`{"version":"9.0.0","headerSize":4096,"blockOffset":4096,`+
		`"indexBlock":{"Start":%d,"End":%d,"Kind":25,"ArraySize":30,"ArrayWidth":0,"CompressionType":1,"Index":null},`+
		`"nextHeaderOffset":%d,"timestamp":"f27f","p1":4096,"p2":%d,"mode":%d}`, start, end, end, start, mode)

	return append([]byte(header+strings.Repeat(" ", 4096-len(header))+data), index.Bytes()...)
}

// TestCloupeDigestMasksRunFields checks a cloupe file's digest ignores what
// two mrp runs differ in (mode, the index block's end, the index's uuids)
// and keeps the data blocks.
func TestCloupeDigestMasksRunFields(t *testing.T) {
	dir := t.TempDir()

	files := map[string][]byte{
		"base.cloupe":  cloupeFile(t, "matrix-block", "ecbb8991-0b28-4454-945f-af66ea557da1", 5370646062688002324),
		"rerun.cloupe": cloupeFile(t, "matrix-block", "1782af85-3022-4f87-b481-ab2237dc4272", 4749635816491279175),
		"data.cloupe":  cloupeFile(t, "matrix-blocK", "ecbb8991-0b28-4454-945f-af66ea557da1", 5370646062688002324),
	}

	digests := map[string]string{}

	for file, raw := range files {
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}

		d, err := stableDigest("", path)
		if err != nil {
			t.Fatal(err)
		}

		digests[file] = d
	}

	if digests["base.cloupe"] != digests["rerun.cloupe"] {
		t.Error("the digest varies with mode, the index end or the index uuids")
	}

	if digests["data.cloupe"] == digests["base.cloupe"] {
		t.Error("the digest misses a data block difference")
	}
}

// TestCloupeDigestRejectsBadLayout checks a header whose index block lies
// past the end of the file fails rather than digesting a part of it.
func TestCloupeDigestRejectsBadLayout(t *testing.T) {
	raw := cloupeFile(t, "matrix-block", "ecbb8991-0b28-4454-945f-af66ea557da1", 1)

	path := filepath.Join(t.TempDir(), "short.cloupe")
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := stableDigest("", path); !errors.Is(err, errCloupeLayout) {
		t.Errorf("err = %v, want %v", err, errCloupeLayout)
	}
}

// TestStableDigestUnknownKind checks a file kind no stable digest covers is
// an error, so a new run-varying output fails the differential.
func TestStableDigestUnknownKind(t *testing.T) {
	if _, err := stableDigest("", "web_summary.html"); !errors.Is(err, errNoStableDigest) {
		t.Errorf("err = %v, want %v", err, errNoStableDigest)
	}
}
