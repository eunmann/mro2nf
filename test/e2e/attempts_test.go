//go:build e2e

package e2e

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestTraceArgs(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantArgs  []string
		wantTrace string
		wantErr   error
	}{
		{"appends the fallback", []string{"-resume"}, []string{"-resume", "-with-trace", "/tmp/t/trace.txt"}, "/tmp/t/trace.txt", nil},
		{"keeps a relative caller trace", []string{"-with-trace", "trace.txt"}, []string{"-with-trace", "trace.txt"}, "/p/trace.txt", nil},
		{"keeps an absolute caller trace", []string{"-with-trace", "/x/tr.txt", "-resume"}, []string{"-with-trace", "/x/tr.txt", "-resume"}, "/x/tr.txt", nil},
		{"rejects a missing file", []string{"-with-trace"}, nil, "", errTraceArg},
		{"rejects a flag as the file", []string{"-with-trace", "-resume"}, nil, "", errTraceArg},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, trace, err := traceArgs("/p", c.args, "/tmp/t/trace.txt")
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}

			if diff := cmp.Diff(c.wantArgs, args); diff != "" {
				t.Errorf("args (-want +got):\n%s", diff)
			}

			if trace != c.wantTrace {
				t.Errorf("trace = %q, want %q", trace, c.wantTrace)
			}
		})
	}
}

func TestCheckAttempts(t *testing.T) {
	ok := []traceRow{
		{Name: "WF:A (1)", Status: "COMPLETED", Exit: 0},
		{Name: "WF:B", Status: "CACHED", Exit: 0},
	}

	if err := checkAttempts(ok); err != nil {
		t.Errorf("completed and cached attempts: %v", err)
	}

	failed := []traceRow{{Name: "WF:C", Status: "FAILED", Exit: 1}, {Name: "WF:D", Status: "ABORTED", Exit: -1}}

	err := checkAttempts(slices.Concat(ok, failed))
	if !errors.Is(err, errFailedAttempt) {
		t.Fatalf("err = %v, want errFailedAttempt", err)
	}

	for _, want := range []string{"WF:C FAILED (exit 1)", "WF:D ABORTED (exit -1)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
