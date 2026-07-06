package emit

import (
	"errors"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
)

// TestCheckProcessCollisions guards #176: qualify() is injective on
// (pipeline, call) but not once an inventory suffix is appended, so a call named
// FOO and a sibling FOO_K (or a fused call X and a sibling X_MN) can emit the
// same name. The check catches every way that collision surfaces.
func TestCheckProcessCollisions(t *testing.T) {
	t.Run("two process decls in one file", func(t *testing.T) {
		files := map[string]string{
			"modules/pipe_P.nf": "process BIND_1_P__FOO_K {\n}\n\nprocess BIND_1_P__FOO_K {\n}\n",
		}
		assertRejects(t, files, "BIND_1_P__FOO_K")
	})

	t.Run("include alias vs process decl in one file", func(t *testing.T) {
		// A fused-split call X aliases STAGE_1_P__X_MN on its include line, while a
		// sibling fused-stage call X_MN declares `process STAGE_1_P__X_MN` — same
		// file, so Nextflow rejects the double binding.
		files := map[string]string{
			"modules/pipe_P.nf": "include { S_MAIN as STAGE_1_P__X_MN; S_JOIN as STAGE_1_P__X_JN } from './stage_S.nf'\n\nprocess STAGE_1_P__X_MN {\n}\n",
		}
		assertRejects(t, files, "STAGE_1_P__X_MN")
	})

	t.Run("same process name across two modules", func(t *testing.T) {
		// Split stage SORT emits process SORT_SPLIT in its module; a plain stage
		// literally named SORT_SPLIT emits its bare process in another module — the
		// bare name collides in Nextflow's global process namespace.
		files := map[string]string{
			"modules/stage_SORT.nf":       "process SORT_SPLIT {\n}\n",
			"modules/stage_SORT_SPLIT.nf": "process SORT_SPLIT {\n}\n",
		}
		assertRejects(t, files, "SORT_SPLIT")
	})

	t.Run("as inside a script block is not an alias", func(t *testing.T) {
		// The include-alias scan must only read `include { ... }` lines, not `as`
		// appearing in an indented process script block.
		files := map[string]string{
			"modules/pipe_P.nf": "include { S_MAIN as STAGE_1_P__A } from './stage_S.nf'\n\nprocess STAGE_1_P__B {\n    script:\n    \"\"\"\n    tar xf x as_needed STAGE_1_P__A\n    \"\"\"\n}\n",
		}
		if err := checkProcessCollisions(files); err != nil {
			t.Errorf("`as` in a script block must not be read as an alias, got %v", err)
		}
	})

	t.Run("distinct names pass", func(t *testing.T) {
		files := map[string]string{
			"modules/pipe_P.nf":  "include { S_MAIN as STAGE_1_P__FOO } from './stage_S.nf'\n\nprocess BIND_1_P__FOO {\n}\n",
			"modules/stage_S.nf": "process S_SPLIT {\n}\n\nprocess S_MAIN {\n}\n",
			"main.nf":            "workflow {\n}\n",
		}
		if err := checkProcessCollisions(files); err != nil {
			t.Errorf("distinct process/alias names must pass, got %v", err)
		}
	})
}

func assertRejects(t *testing.T, files map[string]string, name string) {
	t.Helper()

	err := checkProcessCollisions(files)
	if !errors.Is(err, apperror.ErrUnsupported) {
		t.Fatalf("collision must be rejected as unsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error must name the colliding process %q, got %q", name, err.Error())
	}
}
