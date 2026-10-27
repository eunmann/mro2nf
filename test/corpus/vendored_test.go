package corpus

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// vendoredOrigins are the repository and commit of each vendored source.
// Only MIT sources are vendored; a fetched or licensed corpus runs through
// Env and is never committed.
func vendoredOrigins() map[string][2]string {
	return map[string][2]string{
		"martian":             {"https://github.com/martian-lang/martian", "50ad6c054d307cb6cacb9082425c38c996f216e1"},
		"martian-rust":        {"https://github.com/martian-lang/martian-rust", "6c96615a168e8ab764a7fde99cfd82afca940f2c"},
		"rnascp":              {"https://github.com/nceglia/rnascp", "b16262cc81a1d40ca73968a8bb8e985c7cd2bdac"},
		"tree-sitter-martian": {"https://github.com/LoganAMorrison/tree-sitter-martian", "a113d7785e0e32d52d6eb198d54806299d780c97"},
	}
}

var sourceNote = regexp.MustCompile(`(?m)^Repository: (\S+)\nCommit:\s+([0-9a-f]{40})\nLicense:\s+MIT, in LICENSE`)

// TestVendoredCorpus holds the vendored corpus to its terms: each source
// is a vendoredOrigins entry with the MIT LICENSE it was copied under and a
// SOURCE naming its repository and commit, and Martian accepts every
// vendored file and every invocation of it.
func TestVendoredCorpus(t *testing.T) {
	srcs, err := Vendored(vendoredDir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(srcs))

	for _, s := range srcs {
		names = append(names, s.Name)
		checkOrigin(t, s)
	}

	if d := cmp.Diff(slices.Sorted(maps.Keys(vendoredOrigins())), names); d != "" {
		t.Errorf("vendored sources (-vendoredOrigins +testdata/corpus):\n%s", d)
	}

	entries, err := Load(t.TempDir(), srcs)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.Rejected != nil {
			t.Errorf("Martian refuses %s: %v", e.Name, e.Rejected)
		}
	}

	if len(entries) < 100 {
		t.Errorf("%d vendored invocations, want every vendored file's", len(entries))
	}
}

// checkOrigin requires s's LICENSE to be MIT and its SOURCE to name the
// repository and commit vendoredOrigins pins.
func checkOrigin(t *testing.T, s Source) {
	t.Helper()

	lic, err := os.ReadFile(filepath.Join(s.Roots[0], "LICENSE"))
	if err != nil {
		t.Errorf("%s: %v", s.Name, err)
	} else if !strings.Contains(string(lic), "MIT License") || !strings.Contains(string(lic), "Permission is hereby granted, free of charge") {
		t.Errorf("%s: LICENSE is not the MIT license", s.Name)
	}

	note, err := os.ReadFile(filepath.Join(s.Roots[0], "SOURCE"))
	if err != nil {
		t.Errorf("%s: %v", s.Name, err)

		return
	}

	m := sourceNote.FindStringSubmatch(string(note))
	if m == nil {
		t.Errorf("%s: SOURCE names no repository, commit and MIT license:\n%s", s.Name, note)

		return
	}

	if got, want := [2]string{m[1], m[2]}, vendoredOrigins()[s.Name]; got != want {
		t.Errorf("%s: SOURCE names %v, want %v", s.Name, got, want)
	}
}

// TestCorpusReport logs, per source, the entries Martian accepts and
// refuses, with each refusal: make test-corpus's account of an external
// corpus, whose refusals no table pins.
func TestCorpusReport(t *testing.T) {
	srcs, err := Sources(vendoredDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range srcs {
		entries, err := Load(t.TempDir(), []Source{s})
		if err != nil {
			t.Fatal(err)
		}

		accepted := Accepted(entries)
		t.Logf("%s (%s): %d entries, %d accepted, %d refused by Martian", s.Name, strings.Join(s.Roots, string(filepath.ListSeparator)),
			len(entries), len(accepted), len(entries)-len(accepted))

		for _, e := range entries {
			if e.Rejected != nil {
				t.Logf("  %s: %s", e.Name, strings.Join(strings.Fields(e.Rejected.Error()), " "))
			}
		}
	}
}
