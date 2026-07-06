package shim

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/google/go-cmp/cmp"
)

// TestWriteChunkBundleStagesFilesAndPreservesResources checks the chunk-def
// bundle layout: the chunk's file-typed args are staged under f/ and replaced
// with markers, while the resource overrides survive verbatim under a separate
// "resources" key for the scheduler to read.
func TestWriteChunkBundleStagesFilesAndPreservesResources(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(src, []byte("chunk file"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcJSON, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}

	def := ChunkDef{
		Args:      map[string]json.RawMessage{"f": srcJSON},
		Resources: Resources{MemGB: 2, Threads: 1},
	}
	chunkIn := []ir.Param{{Name: "f", BaseType: "txt", IsFile: true}}

	bundle := filepath.Join(dir, "bundle")
	if err := WriteChunkBundle(bundle, def, chunkIn, types.NewTable(nil)); err != nil {
		t.Fatalf("WriteChunkBundle: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(bundle, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Args      map[string]any `json:"args"`
		Resources Resources      `json:"resources"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse data.json: %v", err)
	}

	wantArgs := map[string]any{"f": FileMarker + filepath.Join("f", "L0000")}
	if diff := cmp.Diff(wantArgs, got.Args); diff != "" {
		t.Errorf("args mismatch (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(def.Resources, got.Resources); diff != "" {
		t.Errorf("resources not preserved (-want +got):\n%s", diff)
	}

	staged, err := os.ReadFile(filepath.Join(bundle, "f", "L0000"))
	if err != nil {
		t.Fatalf("staged leaf: %v", err)
	}
	if string(staged) != "chunk file" {
		t.Errorf("staged leaf content = %q, want %q", staged, "chunk file")
	}
}

// treeContents collects a directory tree as relative-path -> file content, so
// two trees can be compared structurally.
func treeContents(t *testing.T, root string) map[string]string {
	t.Helper()

	got := map[string]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		got[rel] = string(data)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return got
}

// TestCopyTreeDirectoryRecursion checks CopyTree recurses into a directory
// source (a Martian directory output): a nested subdir and its files arrive at
// the destination with identical structure and content.
func TestCopyTreeDirectoryRecursion(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst")
	if err := CopyTree(src, dst); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}

	want := map[string]string{
		"a.txt":                       "alpha",
		filepath.Join("sub", "b.txt"): "beta",
	}
	if diff := cmp.Diff(want, treeContents(t, dst)); diff != "" {
		t.Errorf("copied tree mismatch (-want +got):\n%s", diff)
	}
}

// TestCopyTreeRefusesExistingDest guards the hard-link corruption fence: when
// linking fails (here deterministically, because the destination already
// exists, so link(2) returns EEXIST) CopyTree must refuse to fall back to a
// truncating byte copy over the existing file — that file may be a hard link
// shared with another bundle, and truncation would corrupt the shared inode.
func TestCopyTreeRefusesExistingDest(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(dst, []byte("linked elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := CopyTree(src, dst)
	if !errors.Is(err, errDestExists) {
		t.Fatalf("CopyTree over an existing dst = %v, want errDestExists", err)
	}

	// The existing destination must be untouched (no truncating copy happened).
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "linked elsewhere" {
		t.Errorf("existing dst content = %q, want it untouched", data)
	}
}

// TestMergeArgsChunkPrecedence checks the overlay order of a chunk's _args:
// per-chunk args win over same-named stage args, other stage args survive, and
// the resolved resource __keys are injected.
func TestMergeArgsChunkPrecedence(t *testing.T) {
	chunk := ChunkDef{Args: map[string]json.RawMessage{"n": json.RawMessage("2")}}

	merged, err := mergeArgs(json.RawMessage(`{"n":1,"k":"s"}`), chunk, Resources{MemGB: 1, Threads: 1})
	if err != nil {
		t.Fatalf("mergeArgs: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("parse merged: %v", err)
	}

	for key, want := range map[string]string{
		"n":         "2",   // chunk overrides the stage value
		"k":         `"s"`, // untouched stage arg survives
		"__mem_gb":  "1",
		"__threads": "1",
		"__vmem_gb": "4", // mem + default headroom
	} {
		if string(got[key]) != want {
			t.Errorf("merged[%q] = %s, want %s", key, got[key], want)
		}
	}
}

// TestParseResourcesMalformedValues pins how malformed __-prefixed resource
// values behave: the key is always CONSUMED (never passed through to the data
// args), and an unparseable value leaves the resource at its zero value.
func TestParseResourcesMalformedValues(t *testing.T) {
	tests := []struct {
		name     string
		in       map[string]json.RawMessage
		wantRes  Resources
		wantArgs map[string]json.RawMessage
	}{
		{
			name:     "non-numeric threads consumed and zero",
			in:       map[string]json.RawMessage{"__threads": json.RawMessage(`"x"`), "a": json.RawMessage("1")},
			wantRes:  Resources{},
			wantArgs: map[string]json.RawMessage{"a": json.RawMessage("1")},
		},
		{
			name:     "non-string special consumed and empty",
			in:       map[string]json.RawMessage{"__special": json.RawMessage("5")},
			wantRes:  Resources{},
			wantArgs: map[string]json.RawMessage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, args := parseResources(tt.in)

			if diff := cmp.Diff(tt.wantRes, res); diff != "" {
				t.Errorf("resources mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantArgs, args); diff != "" {
				t.Errorf("args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestResolveResourcesOverlay covers the chunk-over-phase overlay arms not
// already pinned elsewhere: an explicit vmem override below 1 GB is recomputed
// from memory (matching Martian's remote GetSystemReqs floor), and the chunk's
// __special wins over the phase's only when the chunk actually set one.
func TestResolveResourcesOverlay(t *testing.T) {
	tests := []struct {
		name         string
		chunk, phase Resources
		want         Resources
	}{
		{
			name:  "explicit vmem below 1GB recomputed from memory",
			chunk: Resources{VMemGB: 0.5},
			phase: Resources{MemGB: 2},
			want:  Resources{MemGB: 2, VMemGB: 2 + extraVMemGB},
		},
		{
			name:  "chunk special wins over phase",
			chunk: Resources{Special: "chunk"},
			phase: Resources{MemGB: 1, Special: "phase"},
			want:  Resources{MemGB: 1, VMemGB: 1 + extraVMemGB, Special: "chunk"},
		},
		{
			name:  "phase special used when chunk has none",
			chunk: Resources{},
			phase: Resources{MemGB: 1, Special: "phase"},
			want:  Resources{MemGB: 1, VMemGB: 1 + extraVMemGB, Special: "phase"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, resolveResources(tt.chunk, tt.phase)); diff != "" {
				t.Errorf("resolveResources mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// prepAdapterDirs creates the meta/files dirs runAdapter's wrapped path expects
// (it writes _stdout/_stderr into meta and chdirs into files).
func prepAdapterDirs(t *testing.T) (string, string, string) {
	t.Helper()

	work := t.TempDir()
	meta := filepath.Join(work, "meta")
	files := filepath.Join(work, "files")

	for _, d := range []string{meta, files} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return meta, files, filepath.Join(meta, "journal")
}

// TestRunAdapterDispatch covers runAdapter's language dispatch: an exec stage
// runs the stage binary with the phase plus meta/files/journal appended, and
// the unsupported arms (unknown language, comp without an mrjob path) surface
// apperror.ErrUnsupported.
func TestRunAdapterDispatch(t *testing.T) {
	t.Run("exec appends protocol args", func(t *testing.T) {
		if _, err := exec.LookPath("/bin/sh"); err != nil {
			t.Skip("/bin/sh not available")
		}

		meta, files, journal := prepAdapterDirs(t)

		argsOut := filepath.Join(meta, "argv.txt")
		script := filepath.Join(meta, "stage.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$1\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		a := Adapter{Lang: ir.LangExec, Stagecode: script, SrcArgs: []string{argsOut}}
		if err := runAdapter(t.Context(), meta, files, journal, a, "main", Resources{}); err != nil {
			t.Fatalf("runAdapter exec: %v", err)
		}

		raw, err := os.ReadFile(argsOut)
		if err != nil {
			t.Fatalf("stage did not run: %v", err)
		}

		got := strings.Split(strings.TrimSpace(string(raw)), "\n")
		want := []string{argsOut, "main", meta, files, journal}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("exec argv mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("unknown language unsupported", func(t *testing.T) {
		err := runAdapter(t.Context(), t.TempDir(), t.TempDir(), "journal", Adapter{Lang: "cobol"}, "main", Resources{})
		if !errors.Is(err, apperror.ErrUnsupported) {
			t.Errorf("unknown lang = %v, want ErrUnsupported", err)
		}
	})

	t.Run("comp without mrjob unsupported", func(t *testing.T) {
		err := runAdapter(t.Context(), t.TempDir(), t.TempDir(), "journal", Adapter{Lang: ir.LangComp}, "main", Resources{})
		if !errors.Is(err, apperror.ErrUnsupported) {
			t.Errorf("comp without mrjob = %v, want ErrUnsupported", err)
		}
	})
}

// TestResolveStageExe pins that a comp/exec stage binary given as a bare command
// name (no separator) is resolved to an ABSOLUTE path via PATH, so the launched
// process's argv[0] is a real path. Some Martian stage binaries (CellRanger's
// cr_lib) resolve their own executable location from argv[0] and panic on a bare
// name (martian::utils::current_executable). A path with a separator passes
// through unchanged; an unresolvable bare name is returned as-is so the exec
// surfaces a clear not-found error.
func TestResolveStageExe(t *testing.T) {
	if got := resolveStageExe("/abs/tool"); got != "/abs/tool" {
		t.Errorf("resolveStageExe(/abs/tool) = %q, want it unchanged", got)
	}

	if got := resolveStageExe(filepath.Join("dir", "tool")); got != filepath.Join("dir", "tool") {
		t.Errorf("resolveStageExe(dir/tool) = %q, want it unchanged", got)
	}

	if got := resolveStageExe("definitely-not-on-path-xyz"); got != "definitely-not-on-path-xyz" {
		t.Errorf("resolveStageExe(missing) = %q, want the bare name back", got)
	}

	// A bare command found on PATH resolves to an absolute path.
	dir := t.TempDir()
	exe := filepath.Join(dir, "mytool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)

	got := resolveStageExe("mytool")
	if !filepath.IsAbs(got) {
		t.Errorf("resolveStageExe(mytool) = %q, want an absolute path", got)
	}

	if got != exe {
		t.Errorf("resolveStageExe(mytool) = %q, want %q", got, exe)
	}
}

// TestRunAdapterMonitorMemoryQuota drives the full monitored path through
// runAdapter: a stage that allocates well past its mem_gb quota must fail with
// the RETRYABLE errStageFailed (never ErrStageAssert — Nextflow retries memory
// kills with escalated memory) and leave Martian's canonical quota message in
// _errors. Deterministic even for a sub-second child: the exit-time rusage peak
// (recordExitPeak) trips the verdict without depending on the 1s RSS sampler.
func TestRunAdapterMonitorMemoryQuota(t *testing.T) {
	requireLinux(t)

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	meta, files, journal := prepAdapterDirs(t)

	script := filepath.Join(meta, "hog.sh")
	hog := "#!/bin/sh\npython3 -c 'x = bytearray(50*1024*1024)'\n"
	if err := os.WriteFile(script, []byte(hog), 0o755); err != nil {
		t.Fatal(err)
	}

	a := Adapter{Lang: ir.LangExec, Stagecode: script, Monitor: true}

	err := runAdapter(t.Context(), meta, files, journal, a, "main", Resources{MemGB: 0.01})
	if err == nil {
		t.Fatal("over-quota stage succeeded, want a memory-quota failure")
	}
	if !errors.Is(err, errStageFailed) {
		t.Errorf("memory kill = %v, want the retryable errStageFailed", err)
	}
	if errors.Is(err, ErrStageAssert) {
		t.Error("memory kill classified as ErrStageAssert; it must stay retryable")
	}

	msg, readErr := os.ReadFile(filepath.Join(meta, "_errors"))
	if readErr != nil {
		t.Fatalf("_errors not written: %v", readErr)
	}
	if !strings.Contains(string(msg), "exceeded its memory quota") {
		t.Errorf("_errors = %q, want the canonical quota message", msg)
	}
}

// TestWriteChunkDataShape pins the join-phase chunk files' shape: nil chunk
// outs serialize as an empty JSON array (the adapter iterates _chunk_outs, so
// null would break it), and _chunk_defs carries only each chunk's data args —
// the resource overrides are stripped.
func TestWriteChunkDataShape(t *testing.T) {
	meta := t.TempDir()

	defs := []ChunkDef{{
		Args:      map[string]json.RawMessage{"i": json.RawMessage("1")},
		Resources: Resources{MemGB: 5, Threads: 2},
	}}

	if err := writeChunkData(meta, defs, nil); err != nil {
		t.Fatalf("writeChunkData: %v", err)
	}

	outs, err := os.ReadFile(filepath.Join(meta, "_chunk_outs"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(outs)); got != "[]" {
		t.Errorf("_chunk_outs for nil outs = %q, want [] (never null)", got)
	}

	raw, err := os.ReadFile(filepath.Join(meta, "_chunk_defs"))
	if err != nil {
		t.Fatal(err)
	}

	var gotDefs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gotDefs); err != nil {
		t.Fatalf("parse _chunk_defs: %v", err)
	}

	want := []map[string]json.RawMessage{{"i": json.RawMessage("1")}}
	if diff := cmp.Diff(want, gotDefs); diff != "" {
		t.Errorf("_chunk_defs mismatch (resources must be stripped) (-want +got):\n%s", diff)
	}
}
