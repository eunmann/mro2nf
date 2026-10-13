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
// replaced by its path under the mount, <mount>/<bucket>/<key>, a trailing
// / kept, where every task of the run reads it, as mrp hands each stage the
// path it was given. An input with no such value keeps its encoding. With no
// mount, inv is returned as it is.
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

// mountValue is the entry input p's value raw with each s3:// file leaf at
// its mount path, and whether any leaf was one.
func mountValue(tbl *types.Table, p ir.Param, raw json.RawMessage, mount shim.Mount) (json.RawMessage, bool, error) {
	v, err := types.DecodeJSON(raw)
	if err != nil {
		return nil, false, fmt.Errorf("decode entry input %s: %w", p.Name, err)
	}

	changed := false

	nv, err := tbl.Apply([]ir.Param{p}, map[string]any{p.Name: v}, func(s string) (string, error) {
		if m := uriScheme.FindStringSubmatch(s); m == nil || !strings.EqualFold(m[1], "s3") {
			return s, nil
		}

		changed = true

		return mountPath(p.Name, s, mount)
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

// mountPath is the path of the s3:// URI uri of entry input name under the
// mount. Mountpoint for S3 shows no object whose key has an empty, . or ..
// component, so such a URI is refused, as is one naming no bucket. The
// errors name the input, not the value.
func mountPath(name, uri string, mount shim.Mount) (string, error) {
	if len(uri) < len(s3Prefix) || !strings.EqualFold(uri[:len(s3Prefix)], s3Prefix) {
		return "", fmt.Errorf("%w: entry input '%s': not an s3://<bucket>/<key> URI", ErrEntryFile, name)
	}

	rest, dir := strings.CutSuffix(uri[len(s3Prefix):], "/")
	parts := strings.Split(rest, "/")

	if parts[0] == "" {
		return "", fmt.Errorf("%w: entry input '%s': the s3:// URI names no bucket", ErrEntryFile, name)
	}

	for _, c := range parts[1:] {
		if c == "" || c == "." || c == ".." {
			return "", fmt.Errorf("%w: entry input '%s': the s3:// key has an empty, '.' or '..' component, which the S3 mount cannot show", ErrEntryFile, name)
		}
	}

	p := filepath.Join(append([]string{string(mount)}, parts...)...)
	if dir {
		p += "/"
	}

	return p, nil
}

// VetMountedLeaf fails unless the mounted entry file value l names a file
// or directory under a bucket the run's mount shows: a bucket the stack does
// not mount, or a key no object has, would fail a stage later and quietly.
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
		return fmt.Errorf("%w: entry input '%s': bucket %s is not mounted at %s on this run's instances; add it to the stack's mountBuckets",
			ErrEntryFile, l.Input, bucket, at)
	}

	if _, err := os.Stat(l.Value); err != nil {
		return fmt.Errorf("%w: entry input '%s' names no object under the mounted bucket %s", ErrEntryFile, l.Input, bucket)
	}

	return nil
}
