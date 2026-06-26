package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// runPublish finalizes a pipeline's outputs: it copies each file-typed output
// into -dir and rewrites that output's path in the outs JSON to the basename,
// so published results are self-contained and location-independent.
func runPublish(_ context.Context, argv []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	outsFile := fs.String("outs", "", "final outs JSON file")
	files := fs.String("files", "", "comma-separated file-typed output names")
	dir := fs.String("dir", ".", "directory to copy files into and write outs")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	raw, err := readFile(*outsFile)
	if err != nil {
		return err
	}

	outs := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &outs); err != nil {
		return fmt.Errorf("parse outs %s: %w", *outsFile, err)
	}

	for _, name := range splitComma(*files) {
		if err := publishFile(outs, name, *dir); err != nil {
			return err
		}
	}

	out, err := json.MarshalIndent(outs, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal published outs: %w", err)
	}

	return writeRaw(filepath.Join(*dir, "pipeline_outs.json"), out)
}

func publishFile(outs map[string]json.RawMessage, name, dir string) error {
	raw, ok := outs[name]
	if !ok {
		return nil
	}

	// Skip anything that is not a JSON string path (e.g. a null disabled
	// output, or a nested struct/array of files, which M3b does not yet cover).
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '"' {
		return nil
	}

	var src string
	if err := json.Unmarshal(raw, &src); err != nil {
		return fmt.Errorf("decode %s path: %w", name, err)
	}

	if src == "" {
		return nil
	}

	base := filepath.Base(src)
	if err := copyFile(src, filepath.Join(dir, base)); err != nil {
		return err
	}

	rewritten, err := json.Marshal(base)
	if err != nil {
		return fmt.Errorf("rewrite %s: %w", name, err)
	}

	outs[name] = rewritten

	return nil
}

func copyFile(src, dst string) error {
	if src == dst {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}

	return nil
}
