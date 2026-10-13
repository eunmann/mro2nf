package cg

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRecordsRefuseMalformed: an instance stamp or fork record that is
// missing, corrupt, names a node or root the call graph lacks, has a part
// with both or neither of index and key, or whose fork counts do not match
// its parts is refused, never read as some other instance or fork.
func TestRecordsRefuseMalformed(t *testing.T) {
	f := loadFixture(t, "map_fork_upstream")

	write := func(name, body string) string {
		dir := f.next(name)
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), filePerm); err != nil {
			t.Fatal(err)
		}

		return dir
	}

	for name, body := range map[string]string{
		"corrupt": `{"node":`,
		"node":    `{"node":"MFU.NONE","fork":[],"lens":[]}`,
		"root":    `{"node":"MFU.DBL","fork":[{"root":"MFU.NONE","index":0}],"lens":[1]}`,
		"both":    `{"node":"MFU.DBL","fork":[{"root":"MFU.DBL","index":0,"key":"a"}],"lens":[1]}`,
		"neither": `{"node":"MFU.DBL","fork":[{"root":"MFU.DBL"}],"lens":[1]}`,
		"lens":    `{"node":"MFU.DBL","fork":[{"root":"MFU.DBL","key":"a"}],"lens":[]}`,
	} {
		if _, err := f.g.ReadInstance(write(instanceFile, body)); err == nil {
			t.Errorf("instance stamp %s: read without error", name)
		}
	}

	if _, err := f.g.ReadInstance(f.next("empty")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dir without an instance stamp: %v, want ErrNotExist", err)
	}

	for name, body := range map[string]string{
		"corrupt":  `{"fork":`,
		"no parts": `{"fork":[],"lens":[]}`,
		"lens":     `{"fork":[{"root":"MFU.DBL","key":"a"}],"lens":[1,2]}`,
		"root":     `{"fork":[{"root":"MFU.NONE","key":"a"}],"lens":[1]}`,
	} {
		if _, err := f.g.ReadElement(write(elementFile, body)); err == nil {
			t.Errorf("fork element %s: read without error", name)
		}

		if _, err := f.g.DecodeElement(base64.StdEncoding.EncodeToString([]byte(body))); err == nil {
			t.Errorf("fork record %s: decoded without error", name)
		}
	}

	if _, err := f.g.DecodeElement("not base64!"); err == nil {
		t.Error("a fork record that is no base64 decoded without error")
	}

	if err := writeRecord(filepath.Join(write("blocker", ""), "blocker", "el", elementFile), Element{}.encode()); err == nil {
		t.Error("a fork record wrote below a file")
	}

	if _, err := f.g.ReadElement(f.next("empty")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dir without a fork record: %v, want ErrNotExist", err)
	}
}
