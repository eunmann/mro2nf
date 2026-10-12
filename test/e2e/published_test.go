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
	"regexp"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/test/divergence"
	"github.com/google/go-cmp/cmp"
)

// diffPublished compares what the mrp pipestance ps of fixture published with
// mro2nf's results dir, returning one report per differing part. It also
// reports each issue-tracked rule entry scoped to fixture whose removal would
// change nothing, so the entry cannot outlive the divergence it excuses.
func diffPublished(allow divergence.Allowlist, fixture, ps, results string) ([]string, error) {
	reports, err := diffPublishedUnder(allow, fixture, ps, results)
	if err != nil {
		return nil, err
	}

	for _, e := range allow.ScopedRules(divergence.KindPublishedTree, fixture) {
		without, err := diffPublishedUnder(allow.Without(e.ID), fixture, ps, results)
		if err != nil {
			return nil, err
		}

		if slices.Equal(without, reports) {
			reports = append(reports, fmt.Sprintf("allowlist entry %s excuses nothing in %s; delete it or narrow its fixtures", e.ID, fixture))
		}
	}

	return reports, nil
}

func diffPublishedUnder(allow divergence.Allowlist, fixture, ps, results string) ([]string, error) {
	pub, err := loadPublished(allow, fixture, ps, results)
	if err != nil {
		return nil, err
	}

	var reports []string

	if diff := cmp.Diff(pub.mrpTree, pub.nfTree); diff != "" {
		reports = append(reports, "published tree differs (-mrp +nextflow):\n"+diff)
	}

	mrp, err := encodeOrderedJSON(pub.mrpOuts)
	if err != nil {
		return nil, err
	}

	nf, err := encodeOrderedJSON(pub.nfOuts)
	if err != nil {
		return nil, err
	}

	if mrp != nf {
		reports = append(reports, "_outs json mismatch (-mrp(normalized) +nextflow):\n"+cmp.Diff(indentJSON(mrp), indentJSON(nf)))
	}

	return reports, nil
}

// published is what the mrp pipestance of a fixture and mro2nf's results dir
// published, less what the allowlist excuses: each file tree as publishedTree
// gives it, and each outs JSON as decodeOrderedJSON gives it.
type published struct {
	mrpTree, nfTree map[string]string
	mrpOuts, nfOuts any
}

// loadPublished reads what the mrp pipestance ps of fixture and mro2nf's
// results dir published, applying the allowlist entries that cover fixture.
func loadPublished(allow divergence.Allowlist, fixture, ps, results string) (published, error) {
	var (
		pub published
		err error
	)

	var meta []string
	if allow.Excuses(divergence.RuleResultsMetadata, fixture) {
		meta = nfResultsMeta
	}

	followLinks := allow.Excuses(divergence.RuleMrpPublishedSymlinks, fixture)

	if pub.mrpTree, err = publishedTree(filepath.Join(ps, "outs"), followLinks, nil); err != nil {
		return published{}, err
	}

	if pub.nfTree, err = publishedTree(results, false, meta); err != nil {
		return published{}, err
	}

	if pub.mrpOuts, pub.nfOuts, err = loadOutsJSON(allow, fixture, ps, results); err != nil {
		return published{}, err
	}

	return pub, nil
}

// loadOutsJSON decodes the outs JSON of the mrp pipestance ps and mro2nf's
// results/pipeline_outs.json.
func loadOutsJSON(allow divergence.Allowlist, fixture, ps, results string) (any, any, error) {
	nf, err := decodeOutsFile(filepath.Join(results, "pipeline_outs.json"), nfOutsRewrite(allow, fixture))
	if err != nil {
		return nil, nil, err
	}

	mrp, err := mrpOutsJSON(allow, fixture, ps, nf)
	if err != nil {
		return nil, nil, err
	}

	return mrp, nf, nil
}

// unpublishedPath is the placeholder both outs JSONs are read with in place
// of the path of a file left unpublished.
const unpublishedPath = "<unpublished>"

// nfTaskPath matches an absolute path inside a Nextflow task work dir: one
// below the task's <2 hex>/<30 hex> hash dirs. Mirrors NF_TASK_PATH in
// normcmp.py.
var nfTaskPath = regexp.MustCompile(`^/(?:[^/]+/)*[0-9a-f]{2}/[0-9a-f]{30}/.`)

// nfOutsRewrite returns the rewrite of pipeline_outs.json strings the
// allowlist excuses for fixture, nil for none: a path inside a task work dir,
// where mro2nf names a file it left unpublished, becomes the placeholder.
func nfOutsRewrite(allow divergence.Allowlist, fixture string) func(string) string {
	if !allow.Excuses(divergence.RuleUnpublishedPaths, fixture) {
		return nil
	}

	return func(s string) string {
		if nfTaskPath.MatchString(s) {
			return unpublishedPath
		}

		return s
	}
}

// mrpOutsJSON decodes the outs JSON the mrp pipestance ps of fixture writes
// to its top pipeline's fork0 metadata (<pipestance>/<PIPELINE>/fork0/_outs),
// each string rewritten against its counterpart in nf, the decoded
// pipeline_outs.json it is compared with, or nil for none.
func mrpOutsJSON(allow divergence.Allowlist, fixture, ps string, nf any) (any, error) {
	// Glob results come back sorted, so [0] is deterministic.
	matches, err := filepath.Glob(filepath.Join(ps, "*", "fork0", "_outs"))
	if err != nil {
		return nil, fmt.Errorf("glob mrp fork0 _outs: %w", err)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: no mrp fork0 _outs in %s", fs.ErrNotExist, ps)
	}

	rewrite, err := mrpOutsRewrite(allow, fixture, filepath.Join(ps, "outs"))
	if err != nil {
		return nil, err
	}

	v, err := decodeOutsFile(matches[0], nil)
	if err != nil {
		return nil, err
	}

	return pairJSONStrings(v, nf, rewrite), nil
}

// normalizeOutsFile reads an outs JSON file into its compact form, its key
// order kept and its strings passed through rewrite.
func normalizeOutsFile(path string, rewrite func(string) string) (string, error) {
	v, err := decodeOutsFile(path, rewrite)
	if err != nil {
		return "", err
	}

	return encodeOrderedJSON(v)
}

// decodeOutsFile is normalizeOutsFile's value before it is encoded.
func decodeOutsFile(path string, rewrite func(string) string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read outs json: %w", err)
	}

	v, err := decodeOrderedJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}

	if rewrite != nil {
		v, err = mapJSONStrings(v, func(s string) (string, error) { return rewrite(s), nil })
		if err != nil {
			return nil, err
		}
	}

	return v, nil
}

// mrpOutsRewrite returns the rewrite of an mrp outs JSON string, given its
// counterpart in pipeline_outs.json, that the allowlist excuses: the target
// path mrp names for a symlink it published becomes the link's own path, then
// mrpPathRewrite applies. mrp names a target once for each output that
// reaches it and links each output's own path to it, and the pipestance does
// not record which link is whose, so a target with several links becomes the
// one its counterpart names, each link for one reference only; a reference
// left without one keeps the target.
func mrpOutsRewrite(allow divergence.Allowlist, fixture, outs string) (func(string, any) string, error) {
	links := map[string][]string{}

	if allow.Excuses(divergence.RuleMrpPublishedSymlinks, fixture) {
		var err error

		links, err = mrpPublishedLinks(outs)
		if err != nil {
			return nil, err
		}
	}

	rewrite := mrpPathRewrite(allow, fixture, outs)
	claimed := map[string]bool{}

	return func(s string, nf any) string {
		paths := links[s]
		if len(paths) == 1 {
			return rewrite(paths[0])
		}

		for _, p := range paths {
			if !claimed[p] && rewrite(p) == nf {
				claimed[p] = true

				return rewrite(p)
			}
		}

		return rewrite(s)
	}, nil
}

// mrpPathRewrite returns the rewrite of a path in mrp's outs JSON the
// allowlist excuses: a path in the pipestance outside its outs dir, where mrp
// names a file it left unpublished, becomes the placeholder, and the outs dir
// prefix is stripped from a path, since mro2nf names published files relative
// to results/.
func mrpPathRewrite(allow divergence.Allowlist, fixture, outs string) func(string) string {
	inOuts := strings.TrimSuffix(outs, "/") + "/"

	prefix := ""
	if allow.Excuses(divergence.RuleOutsRelativePaths, fixture) {
		prefix = inOuts
	}

	inPs := ""
	if allow.Excuses(divergence.RuleUnpublishedPaths, fixture) {
		inPs = filepath.Dir(strings.TrimSuffix(outs, "/")) + "/"
	}

	return func(s string) string {
		if inPs != "" && strings.HasPrefix(s, inPs) && !strings.HasPrefix(s, inOuts) {
			return unpublishedPath
		}

		if prefix != "" {
			return strings.TrimPrefix(s, prefix)
		}

		return s
	}
}

// mrpPublishedLinks maps each path mrp's outs JSON names for a symlink it
// published under outs to the sorted paths of the symlinks it names that path
// for. mrp names the link's target, except for a stage's relative symlink
// that leads through further links: there outs/ links to the last
// intermediate link and mrp names that link's target
// (martian/core/post_process.go copyOutSymlink, moveOutFile). So both the
// target and, when it is a link, its target map to the symlink. A target
// under outs is the file mrp moved there for the first output that reached it,
// and the path mro2nf names for every output that reaches it too, so it is
// kept.
func mrpPublishedLinks(outs string) (map[string][]string, error) {
	links := map[string][]string{}
	inOuts := strings.TrimSuffix(outs, "/") + "/"

	err := filepath.WalkDir(outs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}

		target, err := readLinkAbs(p)
		if err != nil || strings.HasPrefix(target, inOuts) {
			return err
		}

		links[target] = append(links[target], p)

		if next, err := readLinkAbs(target); err == nil && !strings.HasPrefix(next, inOuts) {
			links[next] = append(links[next], p)
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
