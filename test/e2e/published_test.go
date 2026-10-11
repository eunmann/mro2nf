//go:build e2e

package e2e

// The published-output comparison against mrp: the results/ tree by path and
// content, and the pipeline outs JSON. Each normalization that excuses a real
// difference applies only while its allowlist entry exists.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/eunmann/mro2nf/test/divergence"
	"github.com/google/go-cmp/cmp"
)

// diffPublished compares what the mrp pipestance ps published with mro2nf's
// results dir, returning one report per differing part.
func diffPublished(allow divergence.Allowlist, ps, results string) ([]string, error) {
	outs := filepath.Join(ps, "outs")
	followLinks := allow.Excuses(divergence.RuleMrpPublishedSymlinks)

	var meta []string
	if allow.Excuses(divergence.RuleResultsMetadata) {
		meta = nfResultsMeta
	}

	mrpTree, err := publishedTree(outs, followLinks, nil)
	if err != nil {
		return nil, err
	}

	nfTree, err := publishedTree(results, false, meta)
	if err != nil {
		return nil, err
	}

	var reports []string

	if diff := cmp.Diff(mrpTree, nfTree); diff != "" {
		reports = append(reports, "published tree differs (-mrp +nextflow):\n"+diff)
	}

	r, err := diffOutsJSON(allow, ps, results)
	if err != nil {
		return nil, err
	}

	return append(reports, r...), nil
}

// diffOutsJSON compares mrp's outs JSON, which it writes to the top pipeline's
// fork0 metadata (<pipestance>/<PIPELINE>/fork0/_outs), with mro2nf's
// results/pipeline_outs.json.
func diffOutsJSON(allow divergence.Allowlist, ps, results string) ([]string, error) {
	// Glob results come back sorted, so [0] is deterministic.
	matches, err := filepath.Glob(filepath.Join(ps, "*", "fork0", "_outs"))
	if err != nil {
		return nil, fmt.Errorf("glob mrp fork0 _outs: %w", err)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: no mrp fork0 _outs in %s", fs.ErrNotExist, ps)
	}

	rewrite, err := mrpOutsRewrite(allow, filepath.Join(ps, "outs"))
	if err != nil {
		return nil, err
	}

	sortKeys := allow.Excuses(divergence.RuleOutsSortKeys)

	mrp, err := normalizeOutsFile(matches[0], rewrite, sortKeys)
	if err != nil {
		return nil, err
	}

	nf, err := normalizeOutsFile(filepath.Join(results, "pipeline_outs.json"), nil, sortKeys)
	if err != nil {
		return nil, err
	}

	if mrp == nf {
		return nil, nil
	}

	return []string{"_outs json mismatch (-mrp(normalized) +nextflow):\n" + cmp.Diff(indentJSON(mrp), indentJSON(nf))}, nil
}

// normalizeOutsFile reads an outs JSON file into its compact form, its
// strings passed through rewrite and its object keys sorted if sortKeys.
func normalizeOutsFile(path string, rewrite func(string) string, sortKeys bool) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read outs json: %w", err)
	}

	v, err := decodeOrderedJSON(raw)
	if err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}

	if rewrite != nil {
		v, err = mapJSONStrings(v, func(s string) (string, error) { return rewrite(s), nil })
		if err != nil {
			return "", err
		}
	}

	if sortKeys {
		v = sortJSONKeys(v)
	}

	return encodeOrderedJSON(v)
}

// mrpOutsRewrite returns the rewrite of mrp's outs JSON strings the
// allowlist excuses: the target path mrp names for a symlink it published
// becomes the link's own path, and the outs dir prefix is stripped from a
// path, since mro2nf names published files relative to results/.
func mrpOutsRewrite(allow divergence.Allowlist, outs string) (func(string) string, error) {
	links := map[string]string{}

	if allow.Excuses(divergence.RuleMrpPublishedSymlinks) {
		var err error

		links, err = mrpPublishedLinks(outs)
		if err != nil {
			return nil, err
		}
	}

	prefix := ""
	if allow.Excuses(divergence.RuleOutsRelativePaths) {
		prefix = strings.TrimSuffix(outs, "/") + "/"
	}

	return func(s string) string {
		if link, ok := links[s]; ok {
			s = link
		}

		if prefix != "" {
			return strings.TrimPrefix(s, prefix)
		}

		return s
	}, nil
}

// mrpPublishedLinks maps the path mrp's outs JSON names for each symlink it
// published under outs to the symlink's path. mrp names the link's target,
// except for a stage's relative symlink that leads through further links:
// there outs/ links to the last intermediate link and mrp names that link's
// target (martian/core/post_process.go copyOutSymlink, moveOutFile). So both
// the target and, when it is a link, its target map to the symlink.
func mrpPublishedLinks(outs string) (map[string]string, error) {
	links := map[string]string{}

	err := filepath.WalkDir(outs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}

		target, err := readLinkAbs(p)
		if err != nil {
			return err
		}

		links[target] = p

		if next, err := readLinkAbs(target); err == nil {
			links[next] = p
		}

		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read mrp published links: %w", err)
	}

	return links, nil
}

// readLinkAbs returns the target of the symlink p, a relative target joined
// to p's directory.
func readLinkAbs(p string) (string, error) {
	target, err := os.Readlink(p)
	if err != nil {
		return "", fmt.Errorf("read link: %w", err)
	}

	if filepath.IsAbs(target) {
		return target, nil
	}

	return filepath.Clean(filepath.Join(filepath.Dir(p), target)), nil
}
