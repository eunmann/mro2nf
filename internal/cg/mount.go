package cg

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/internal/ir"
	"github.com/eunmann/mro2nf/internal/shim"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/core"
)

// s3Prefix begins an s3:// URI, in any case, as Nextflow reads its scheme.
const s3Prefix = "s3://"

// MountEntryValues is inv with every s3:// file value of its entry inputs
// whose bucket is a directory <mount>/<bucket> where PLAN runs replaced by
// its path there, <mount>/<bucket>/<key>, a trailing / kept, where every
// task of the run reads it, as mrp hands each stage the path it was given.
// A value of any other bucket stays the URI it is, which Nextflow stages:
// the mount spares a download, and no run requires it. An input with no
// mounted value keeps its encoding. With no mount, inv is returned as it is.
func MountEntryValues(src Source, inv *core.InvocationData, mroPaths []string, mount shim.Mount) (*core.InvocationData, error) {
	if mount == "" {
		return inv, nil
	}

	man, err := src.Manifest(mroPaths)
	if err != nil {
		return nil, err
	}

	params, err := man.Params(inv.Call, types.RoleIn)
	if err != nil {
		return nil, fmt.Errorf("entry params: %w", err)
	}

	out := *inv
	out.Args = maps.Clone(inv.Args)

	for _, p := range params {
		raw, ok := inv.Args[p.Name]
		if !ok {
			continue
		}

		mounted, changed, err := mountValue(man.Table(), p, raw, mount)
		if err != nil {
			return nil, err
		}

		if changed {
			out.Args[p.Name] = mounted
		}
	}

	return &out, nil
}

// mountValue is the entry input p's value raw with each s3:// file leaf of
// a mounted bucket at its mount path, and whether any leaf was one.
func mountValue(tbl *types.Table, p ir.Param, raw json.RawMessage, mount shim.Mount) (json.RawMessage, bool, error) {
	v, err := types.DecodeJSON(raw)
	if err != nil {
		return nil, false, fmt.Errorf("decode entry input %s: %w", p.Name, err)
	}

	changed := false

	nv, err := tbl.Apply([]ir.Param{p}, map[string]any{p.Name: v}, func(s string) (string, error) {
		at, ok, err := mountPath(p.Name, s, mount)
		if err != nil {
			return "", err
		}

		if !ok {
			return s, nil
		}

		changed = true

		return at, nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("mount entry values: %w", err)
	}

	if !changed {
		return raw, false, nil
	}

	enc, err := json.Marshal(nv[p.Name])
	if err != nil {
		return nil, false, fmt.Errorf("encode entry input %s: %w", p.Name, err)
	}

	return enc, true, nil
}

// mountPath is the path under the mount of the file value uri of entry
// input name, and whether it has one: an s3://<bucket>/<key> URI whose
// bucket the mount shows. Mountpoint for S3 shows no object whose key has an
// empty, . or .. component, so such a URI of a mounted bucket is refused.
// The errors name the input, not the value.
func mountPath(name, uri string, mount shim.Mount) (string, bool, error) {
	if len(uri) < len(s3Prefix) || !strings.EqualFold(uri[:len(s3Prefix)], s3Prefix) {
		return "", false, nil
	}

	rest, dir := strings.CutSuffix(uri[len(s3Prefix):], "/")
	parts := strings.Split(rest, "/")

	if !bucketMounted(parts[0], mount) {
		return "", false, nil
	}

	for _, c := range parts[1:] {
		if c == "" || c == "." || c == ".." {
			return "", false, fmt.Errorf("%w: entry input '%s': the s3:// key has an empty, '.' or '..' component, which the S3 mount cannot show", ErrEntryFile, name)
		}
	}

	p := filepath.Join(append([]string{string(mount)}, parts...)...)
	if dir {
		p += "/"
	}

	return p, true, nil
}

// bucketMounted reports whether the mount shows bucket: <mount>/<bucket> is
// a directory where PLAN runs, on the compute environment every task runs
// on. Any directory counts, not only a mount point, as a compute environment
// may serve the root as one bind-mounted tree. A name that is no single path
// component is no bucket. Whatever the mount does not show is staged, which
// every run can do.
func bucketMounted(bucket string, mount shim.Mount) bool {
	if mount == "" || bucket == "" || bucket == "." || bucket == ".." {
		return false
	}

	fi, err := os.Stat(filepath.Join(string(mount), bucket))

	return err == nil && fi.IsDir()
}

// VetMountedLeaf fails unless the mounted entry file value l names a file
// or directory under a bucket the run's mount shows: a key no object has,
// or a path given under the mount of a bucket it does not show, would fail
// a stage later and quietly.
func VetMountedLeaf(l EntryLeaf, mount shim.Mount) error {
	rel, err := filepath.Rel(string(mount), filepath.Clean(l.Value))
	if err != nil {
		return fmt.Errorf("%w: entry input '%s': %w", ErrEntryFile, l.Input, err)
	}

	bucket, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if bucket == "." {
		return fmt.Errorf("%w: entry input '%s' names the S3 mount root %s, not a bucket under it", ErrEntryFile, l.Input, mount)
	}

	at := filepath.Join(string(mount), bucket)
	if _, err := os.Stat(at); err != nil {
		return fmt.Errorf("%w: entry input '%s': bucket %s is not mounted at %s on this run's instances; mount it there, or pass the input as an s3:// URI, which the tasks stage if their role may read the bucket",
			ErrEntryFile, l.Input, bucket, at)
	}

	if _, err := os.Stat(l.Value); err != nil {
		return fmt.Errorf("%w: entry input '%s' names no object under the mounted bucket %s", ErrEntryFile, l.Input, bucket)
	}

	return nil
}
