// Package fileio writes and reads the files mro2nf and mre create, with the
// modes mrp gives its own.
package fileio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// The modes of the dirs and files mro2nf and mre create. mrp creates its
// dirs 0777 and its files 0644 (martian/util/util.go Mkdir, MkdirAll;
// martian/core/metadata.go _writeRawNoLock); under the usual umask 022 those dirs
// are DirPerm.
const (
	DirPerm  = 0o755
	FilePerm = 0o644
	ExecPerm = 0o755
)

// Write writes data to path, creating its dir.
func Write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), DirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, FilePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// WriteJSON writes v to path as compact JSON, creating its dir.
func WriteJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	return Write(path, raw)
}

// ReadJSON decodes the JSON at path into v.
func ReadJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}
