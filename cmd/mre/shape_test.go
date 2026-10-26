package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lsMRO hands MK's directory to LS, which lists it.
const lsMRO = `stage MK(
    out path d,
    src exec "mkd.sh",
)

stage LS(
    in  path   d,
    out string names,
    src exec   "ls.sh",
)

pipeline P(
    out string names,
)
{
    call MK()

    call LS(
        d = MK.d,
    )

    return (
        names = LS.names,
    )
}

call P()
`

// mkdScript is MK's exec code: d holds a file, empty dirs at two depths
// and a link that reaches nothing; lsScript is LS's: names lists d's every
// entry, its type and a link's target.
const (
	mkdScript = `#!/bin/sh
meta=$2 files=$3
mkdir -p "$files/d/sub" "$files/d/empty" "$files/d/deep/er"
printf 'x' > "$files/d/sub/x.txt"
ln -s /nonexistent "$files/d/dangling"
echo "{\"d\":\"$files/d\"}" > "$meta/_outs"
touch "$meta/_complete"
`
	lsScript = `#!/bin/sh
meta=$2
d=$(tr -d ' \n' < "$meta/_args" | sed 's/.*"d":"\([^"]*\)".*/\1/')
names=$(cd "$d" && find . -mindepth 1 -printf '%p:%y:%l\n' | sort | paste -sd ' ' -)
echo "{\"names\":\"$names\"}" > "$meta/_outs"
touch "$meta/_complete"
`
)

// TestStageRestoresWhatAnObjectStoreDrops (#624): LS's task stages MK's
// directory as an object store hands it over, its files alone, and its
// stage lists the empty dirs and the link that reaches nothing MK wrote,
// as mrp's LS lists them.
func TestStageRestoresWhatAnObjectStoreDrops(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	mro := writeTestFile(t, filepath.Join(dir, "pipeline.mro"), lsMRO)

	for name, src := range map[string]string{"mkd.sh": mkdScript, "ls.sh": lsScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(work)

	if _, err := mreIn(t, mro, unsplitTask("P.MK", "mk")...); err != nil {
		t.Fatalf("MK: %v", err)
	}

	filesOnly(t, filepath.Join("mk", "outs", "d"), filepath.Join("ls", "in", "u1"))

	if _, err := mreIn(t, mro, unsplitTask("P.LS", "ls", "-in", "ls/in", "-refs", "ls/refs")...); err != nil {
		t.Fatalf("LS: %v", err)
	}

	want := `names:"./dangling:l:/nonexistent ./deep:d: ./deep/er:d: ./empty:d: ./sub:d: ./sub/x.txt:f:"`
	if got := postedValues(t, filepath.Join("ls", "values")); got != want {
		t.Errorf("LS posted %s, want %s", got, want)
	}
}

// filesOnly copies the regular files of the tree at src to dst, as an
// upload to an object store and a download from it leave the tree.
func filesOnly(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		writeOutsFile(t, filepath.Join(dst, strings.TrimPrefix(path, src)), string(raw))

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
