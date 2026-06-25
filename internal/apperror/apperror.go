// Package apperror defines the sentinel and typed errors used across the
// transpiler so callers can classify failures with errors.Is / errors.As.
package apperror

import (
	"errors"
	"fmt"
)

// ErrParse indicates Martian source failed to parse or type-check.
var ErrParse = errors.New("parse")

// ErrUnsupported indicates a Martian construct the transpiler cannot lower to
// Nextflow.
var ErrUnsupported = errors.New("unsupported construct")

// UnsupportedError reports a specific Martian construct that has no Nextflow
// lowering yet. It unwraps to ErrUnsupported.
type UnsupportedError struct {
	// Construct is the Martian feature name, e.g. "map call" or "sweep".
	Construct string
	// Detail optionally explains why or where it was encountered.
	Detail string
}

func (e *UnsupportedError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("unsupported construct %q", e.Construct)
	}

	return fmt.Sprintf("unsupported construct %q: %s", e.Construct, e.Detail)
}

// Unwrap ties UnsupportedError to the ErrUnsupported sentinel.
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }
