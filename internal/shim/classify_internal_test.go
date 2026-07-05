package shim

import (
	"errors"
	"io"
	"testing"
)

// errWaitFail stands in for a non-zero cmd.Wait result.
var errWaitFail = errors.New("exit status 1")

// TestClassifyAdapterResult pins the py-adapter verdict classification,
// especially the fd-4 read-failure arm: if the error channel could not be
// drained, a stage failure message may have been lost, so the phase must fail
// loudly even when the process exited 0.
func TestClassifyAdapterResult(t *testing.T) {
	cases := []struct {
		name             string
		stageErr         string
		readErr, waitErr error
		wantErr          bool
		wantIs           error
	}{
		{name: "clean pass"},
		{
			name:    "read failure with clean exit must not pass",
			readErr: io.ErrClosedPipe,
			wantErr: true,
			wantIs:  io.ErrClosedPipe,
		},
		{
			name:     "stage message is authoritative over read failure",
			stageErr: "boom",
			readErr:  io.ErrClosedPipe,
			wantErr:  true,
			wantIs:   errStageFailed,
		},
		{
			name:     "assert classified from partial channel data",
			stageErr: assertPrefix + "invariant broken",
			wantErr:  true,
			wantIs:   ErrStageAssert,
		},
		{
			name:    "nonzero exit without message",
			waitErr: errWaitFail,
			wantErr: true,
			wantIs:  errWaitFail,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyAdapterResult("main", t.TempDir(), []byte(tc.stageErr), tc.readErr, tc.waitErr, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("classifyAdapterResult error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("classifyAdapterResult error = %v, want errors.Is %v", err, tc.wantIs)
			}
		})
	}
}
