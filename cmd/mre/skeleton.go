package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eunmann/mro2nf/internal/shim"
)

// expandSkeletons expands, in place, each bundle skeleton staged among
// paths: one a path names, or one in the dir a path names. A staged dir is
// a bundle or a dir of them, and is left as it is.
func expandSkeletons(paths []string) error {
	for _, p := range paths {
		if p == "" {
			continue
		}

		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("staged %s: %w", p, err)
		}

		if !info.IsDir() {
			if err := expandSkeleton(p); err != nil {
				return err
			}

			continue
		}

		if err := expandSkeletonsIn(p); err != nil {
			return err
		}
	}

	return nil
}

func expandSkeletonsIn(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read staged %s: %w", dir, err)
	}

	for _, e := range entries {
		at := filepath.Join(dir, e.Name())

		info, err := os.Stat(at)
		if err != nil {
			return fmt.Errorf("staged %s: %w", at, err)
		}

		if info.IsDir() {
			continue
		}

		if err := expandSkeleton(at); err != nil {
			return err
		}
	}

	return nil
}

// expandSkeleton replaces the skeleton staged as item with the bundle it
// describes.
func expandSkeleton(item string) error {
	staged := item + shim.SkeletonFile
	if err := os.Rename(item, staged); err != nil {
		return fmt.Errorf("expand %s: %w", item, err)
	}

	if err := shim.ExpandSkeleton(staged, item); err != nil {
		return fmt.Errorf("expand %s: %w", item, err)
	}

	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("expand %s: %w", item, err)
	}

	return nil
}
