// Command gendoc rewrites the generated divergence table of
// docs/FEATURE_COVERAGE.md from the allowlist test/e2e/divergences.json
// (make docs-divergences, run from the repo root).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/eunmann/mro2nf/test/divergence"
)

func main() {
	allowlist := flag.String("allowlist", "test/e2e/divergences.json", "divergence allowlist to render")
	doc := flag.String("doc", "docs/FEATURE_COVERAGE.md", "document holding the generated table")
	flag.Parse()

	if err := divergence.WriteDoc(*allowlist, *doc); err != nil {
		fmt.Fprintln(os.Stderr, "gendoc:", err)
		os.Exit(1)
	}
}
