// Package docs checks the repository's Markdown: every relative link names a
// file that exists, and every #fragment a heading of the page it names, as
// GitHub renders them.
package docs

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

const repoRoot = "../.."

var (
	linkRE     = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)
	headingRE  = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)
	codeSpanRE = regexp.MustCompile("`[^`]*`")
)

// TestDocLinksResolve fails on a relative link, in any Markdown file of the
// repository, to a missing file or to a heading its page does not have.
func TestDocLinksResolve(t *testing.T) {
	pages := markdownFiles(t)
	if len(pages) == 0 {
		t.Fatal("found no Markdown files")
	}

	anchors := map[string]map[string]bool{}

	for _, page := range pages {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}

		for _, target := range links(prose(string(raw))) {
			if err := resolve(page, target, anchors); err != "" {
				rel, _ := filepath.Rel(repoRoot, page)
				t.Errorf("%s: link %q: %s", rel, target, err)
			}
		}
	}
}

// markdownFiles are the .md files under the repository root, outside hidden
// directories, node_modules and artifacts.
func markdownFiles(t *testing.T) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := d.Name()
		if d.IsDir() && path != repoRoot && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "artifacts") {
			return filepath.SkipDir
		}

		if !d.IsDir() && strings.HasSuffix(name, ".md") {
			out = append(out, path)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// prose is the lines of doc outside fenced code blocks, so a link or
// heading inside one is not read as one.
func prose(doc string) []string {
	var out []string

	fenced := false

	for line := range strings.Lines(doc) {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced

			continue
		}

		if !fenced {
			out = append(out, strings.TrimRight(line, "\r\n"))
		}
	}

	return out
}

// links are the targets of the relative links in lines, outside code spans.
func links(lines []string) []string {
	var out []string

	for _, line := range lines {
		for _, m := range linkRE.FindAllStringSubmatch(codeSpanRE.ReplaceAllString(line, ""), -1) {
			target := m[2]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}

			out = append(out, target)
		}
	}

	return out
}

// resolve is why target, a link in page, does not resolve, or "".
func resolve(page, target string, anchors map[string]map[string]bool) string {
	path, frag, _ := strings.Cut(target, "#")

	dest := page
	if path != "" {
		dest = filepath.Join(filepath.Dir(page), path)
		if _, err := os.Stat(dest); err != nil {
			return "no such file"
		}
	}

	if frag == "" {
		return ""
	}

	if !strings.HasSuffix(dest, ".md") {
		return "a fragment of a page that is not Markdown"
	}

	if anchors[dest] == nil {
		raw, err := os.ReadFile(dest)
		if err != nil {
			return err.Error()
		}

		anchors[dest] = headingAnchors(prose(string(raw)))
	}

	if !anchors[dest][frag] {
		return "no heading with this anchor"
	}

	return ""
}

// headingAnchors are the anchors GitHub gives the headings in lines: each
// heading's slug, and a repeated slug's -1, -2 and on.
func headingAnchors(lines []string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}

	for _, line := range lines {
		m := headingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		slug := slugify(m[1])
		if n := seen[slug]; n > 0 {
			out[slug+"-"+strconv.Itoa(n)] = true
		} else {
			out[slug] = true
		}

		seen[slug]++
	}

	return out
}

// slugify is GitHub's anchor for heading text: lower case, with every
// character but a letter, a digit, a space, '-' or '_' dropped, and each
// space a '-'. Code spans keep their text; link targets are dropped.
func slugify(heading string) string {
	heading = linkRE.ReplaceAllString(heading, "$1")

	var b strings.Builder

	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}

	return b.String()
}

// TestSlugify pins slugify to the anchors GitHub gives the headings these
// docs link to.
func TestSlugify(t *testing.T) {
	for heading, want := range map[string]string{
		"`mrp --overrides`":            "mrp---overrides",
		"`job_resources`":              "job_resources",
		"`--onfinish`":                 "--onfinish",
		"Divergences from `mrp`":       "divergences-from-mrp",
		"Language — writing-pipelines": "language--writing-pipelines",
		"A [linked](x.md) heading":     "a-linked-heading",
		"Mounted `s3://` values":       "mounted-s3-values",
	} {
		if got := slugify(heading); got != want {
			t.Errorf("slugify(%q) = %q, want %q", heading, got, want)
		}
	}
}
