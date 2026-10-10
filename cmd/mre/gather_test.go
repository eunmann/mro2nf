package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestOrderByIndexMatchesSortV proves the gather order is the order the
// generated scripts used to get from `ls | sort -V`, for every name shape and
// across the five-digit padding boundary (index 100000 is six digits, which a
// plain lexical sort would misplace): the index order and GNU version sort
// agree entry for entry.
func TestOrderByIndexMatchesSortV(t *testing.T) {
	// The name shapes the emitter stages into a gather, as fmt patterns over
	// the writer-assigned index: a plain fork's outs bundle, a nested map's
	// composite outer~inner keys (one and two outer levels, all sharing the
	// outer prefix within one MERGE_K), a split's chunk defs, and the MAIN
	// outs named out_${chunk.baseName}.
	shapes := []struct{ name, pattern string }{
		{"fork outs", forkOutPrefix + "fork_%05d"},
		{"composite fork outs", forkOutPrefix + "fork_00007~fork_%05d"},
		{"nested composite fork outs", forkOutPrefix + "fork_00002~fork_00011~fork_%05d"},
		{"chunk defs", chunkDefPrefix + "%05d"},
		{"chunk outs", chunkOutPrefix + "%05d"},
	}

	for _, shape := range shapes {
		for _, n := range []int{12, 100_001} {
			t.Run(fmt.Sprintf("%s/%d", shape.name, n), func(t *testing.T) {
				names := make([]string, n)
				for i := range names {
					names[i] = fmt.Sprintf(shape.pattern, i)
				}

				rng := rand.New(rand.NewPCG(1, uint64(n)))
				rng.Shuffle(n, func(i, j int) { names[i], names[j] = names[j], names[i] })

				got, err := orderByIndex("d", names)
				if err != nil {
					t.Fatalf("orderByIndex: %v", err)
				}

				for i := range got {
					got[i] = filepath.Base(got[i])
				}

				if diff := cmp.Diff(sortV(t, names), got); diff != "" {
					t.Errorf("index order differs from sort -V (-sortV +index):\n%s", diff)
				}
			})
		}
	}
}

// sortV orders names with GNU `sort -V`, the order the gathers had before mre
// listed the staged dirs itself.
func sortV(t *testing.T, names []string) []string {
	t.Helper()

	cmd := exec.Command("sort", "-V")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sort -V: %v", err)
	}

	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// TestGatherIndexed lists a staged dir: only prefixed entries are gathered,
// in index order, and a missing dir is the zero-entry gather an empty
// Nextflow list stages.
func TestGatherIndexed(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"outs__fork_00002", "outs__fork_00000", "outs__fork_00001", "forkmeta.json", "types.json"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := gatherIndexed(dir, forkOutPrefix, 3)
	if err != nil {
		t.Fatalf("gatherIndexed: %v", err)
	}

	want := []string{filepath.Join(dir, "outs__fork_00000"), filepath.Join(dir, "outs__fork_00001"), filepath.Join(dir, "outs__fork_00002")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gathered paths mismatch (-want +got):\n%s", diff)
	}

	for _, d := range []string{t.TempDir(), filepath.Join(dir, "absent")} {
		got, err := gatherIndexed(d, forkOutPrefix, 0)
		if err != nil || len(got) != 0 {
			t.Errorf("gatherIndexed(%s, 0) = %v, %v; want no entries, no error", d, got, err)
		}
	}
}

// TestGatherIndexedErrors is the #324 guard at the listing seam: a count that
// differs from the sidecar's, a name without the writer's index, a repeated or
// out-of-range index, mixed stems, and an unlistable dir all fail loudly.
func TestGatherIndexedErrors(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    int
		wantErr error
	}{
		{name: "missing entry", entries: []string{"outs__fork_00000"}, want: 2, wantErr: errGatherCount},
		{name: "extra entry", entries: []string{"outs__fork_00000", "outs__fork_00001"}, want: 1, wantErr: errGatherCount},
		{name: "none staged but some expected", want: 1, wantErr: errGatherCount},
		{name: "no trailing index", entries: []string{"outs__fork_none"}, want: 1, wantErr: errGatherEntry},
		{name: "repeated index", entries: []string{"outs__fork_00001", "outs__fork_1"}, want: 2, wantErr: errGatherEntry},
		{name: "index past count", entries: []string{"outs__fork_00000", "outs__fork_00005"}, want: 2, wantErr: errGatherEntry},
		{name: "mixed stems", entries: []string{"outs__a~fork_00000", "outs__b~fork_00001"}, want: 2, wantErr: errGatherEntry},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.entries {
				if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := gatherIndexed(dir, forkOutPrefix, tc.want); !errors.Is(err, tc.wantErr) {
				t.Errorf("gatherIndexed: err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	notDir := writeTestFile(t, filepath.Join(t.TempDir(), "file"), "x")
	if _, err := gatherIndexed(notDir, forkOutPrefix, 0); err == nil {
		t.Error("gatherIndexed on an unlistable path must fail, not gather zero entries")
	}
}

// TestReadChunkData gathers a JOIN's staged chunk defs and outs from one task
// dir in chunk-index order, and a zero-chunk split (chunks.json [] or null, no
// bundles staged) joins over no chunks.
func TestReadChunkData(t *testing.T) {
	dir := t.TempDir()
	for _, i := range []int{10, 0, 11, 1, 2, 3, 4, 5, 6, 7, 8, 9} {
		writeTestBundle(t, filepath.Join(dir, fmt.Sprintf("chunk_%05d", i)), map[string]any{"args": map[string]any{"i": i}})
		writeTestBundle(t, filepath.Join(dir, fmt.Sprintf("out_chunk_%05d", i)), map[string]any{"part": i})
	}

	chunks := writeTestFile(t, filepath.Join(dir, "chunks.json"), "["+strings.Repeat("{},", 11)+"{}]")

	defs, outs, err := readChunkData(chunks, dir)
	if err != nil {
		t.Fatalf("readChunkData: %v", err)
	}

	for i := range 12 {
		if got := string(defs[i].Args["i"]); got != strconv.Itoa(i) {
			t.Errorf("def %d carries chunk %s", i, got)
		}

		if want := fmt.Sprintf(`{"part":%d}`, i); string(outs[i]) != want {
			t.Errorf("out %d = %s, want %s", i, outs[i], want)
		}
	}

	for _, empty := range []string{"[]", "null"} {
		zero := t.TempDir()
		chunks := writeTestFile(t, filepath.Join(zero, "chunks.json"), empty)

		defs, outs, err := readChunkData(chunks, zero)
		if err != nil || len(defs) != 0 || len(outs) != 0 {
			t.Errorf("zero-chunk join (%s): %d defs, %d outs, err %v; want none", empty, len(defs), len(outs), err)
		}
	}
}

// TestReadChunkDataCountMismatch is the #324 guard for JOIN: chunk defs or
// chunk outs that do not number the split's chunks.json fail loudly instead of
// joining a short (or empty) chunk set.
func TestReadChunkDataCountMismatch(t *testing.T) {
	tests := []struct {
		name    string
		defs    int
		outs    int
		chunks  string // empty = no -chunksfile
		wantErr error
	}{
		{name: "chunk outs missing", defs: 3, outs: 0, chunks: "[{},{},{}]", wantErr: errGatherCount},
		{name: "one chunk out short", defs: 3, outs: 2, chunks: "[{},{},{}]", wantErr: errGatherCount},
		{name: "chunk defs missing", defs: 0, outs: 3, chunks: "[{},{},{}]", wantErr: errGatherCount},
		{name: "more chunks than defs", defs: 2, outs: 2, chunks: "[{}]", wantErr: errGatherCount},
		{name: "no chunks file", defs: 1, outs: 1, wantErr: errNoChunksFile},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for i := range tc.defs {
				writeTestBundle(t, filepath.Join(dir, fmt.Sprintf("chunk_%05d", i)), map[string]any{})
			}

			for i := range tc.outs {
				writeTestBundle(t, filepath.Join(dir, fmt.Sprintf("out_chunk_%05d", i)), map[string]any{})
			}

			chunks := ""
			if tc.chunks != "" {
				chunks = writeTestFile(t, filepath.Join(dir, "chunks.json"), tc.chunks)
			}

			if _, _, err := readChunkData(chunks, dir); !errors.Is(err, tc.wantErr) {
				t.Errorf("readChunkData: err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
