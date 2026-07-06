package emit

import (
	"errors"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/apperror"
)

// TestCheckDupProcesses guards #176: qualify() is injective on (pipeline, call)
// but not once an inventory suffix is appended — qualify(P,C)+"_K" ==
// qualify(P,C+"_K"), so a call named FOO and a sibling FOO_K emit the same keyed
// process name into one module, and the whole project fails to parse. Reject it
// at transpile time with the colliding name.
func TestCheckDupProcesses(t *testing.T) {
	dup := "process BIND_1_P__FOO_K {\n}\n\nprocess OTHER {\n}\n\nprocess BIND_1_P__FOO_K {\n}\n"
	err := checkDupProcesses(dup, "pipe_P.nf")
	if !errors.Is(err, apperror.ErrUnsupported) {
		t.Fatalf("duplicate process must be rejected as unsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "BIND_1_P__FOO_K") {
		t.Errorf("error must name the colliding process, got %q", err.Error())
	}

	ok := "process BIND_1_P__FOO {\n}\n\nprocess BIND_1_P__FOO_K {\n}\n"
	if err := checkDupProcesses(ok, "pipe_P.nf"); err != nil {
		t.Errorf("distinct process names must pass, got %v", err)
	}
}
