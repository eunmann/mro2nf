package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
	"github.com/martian-lang/martian/martian/core"
)

// TestChunksDirRoundTrip: the chunks dir a split's job writes reads back as
// the split's stage defs, in chunk order under mrp's chunk names, each
// def's args and resources and the join's resources intact.
func TestChunksDirRoundTrip(t *testing.T) {
	defs := &core.StageDefs{JoinDef: &core.JobResources{Threads: 2, MemGB: 3}}
	for i := range 11 {
		res := &core.JobResources{MemGB: float64(i + 1)}
		defs.ChunkDefs = append(defs.ChunkDefs, &core.ChunkDef{Args: core.LazyArgumentMap{"i": json.RawMessage([]byte{'0' + byte(i%10)})}, Resources: res})
	}

	dir := filepath.Join(t.TempDir(), "chunks")
	if err := writeChunksDir(dir, defs, types.Manifest{}, "S"); err != nil {
		t.Fatal(err)
	}

	got, names, err := readChunksDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if names[0] != "chnk00" || names[10] != "chnk10" || len(names) != 11 {
		t.Errorf("chunk names %v, mrp names 11 chunks chnk00..chnk10", names)
	}

	want, err := json.Marshal(defs)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}

	if d := cmp.Diff(string(want), string(raw)); d != "" {
		t.Errorf("stage defs differ after the round trip (-split +read):\n%s", d)
	}

	c, err := readChunk(filepath.Join(dir, names[3]))
	if err != nil {
		t.Fatal(err)
	}

	if c.Index != 3 || c.Count != 11 {
		t.Errorf("chunk %s is %d of %d, want 3 of 11", names[3], c.Index, c.Count)
	}
}
