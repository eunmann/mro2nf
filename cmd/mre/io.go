package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const filePerm = 0o644

// readFile reads a JSON file, returning nil for an empty path.
func readFile(path string) (json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return data, nil
}

func writeRaw(path string, data []byte) error {
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
