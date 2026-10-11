package e2e

import (
	"os"
	"strings"
	"testing"
)

// logicalLines joins backslash-continued lines, so a `RUN A=1 \` + `go build`
// pair reads as the one command it is.
func logicalLines(src string) []string {
	var out []string

	cur := ""

	for line := range strings.Lines(src) {
		line = strings.TrimRight(line, "\r\n")
		if s, ok := strings.CutSuffix(line, "\\"); ok {
			cur += s

			continue
		}

		out = append(out, cur+line)
		cur = ""
	}

	if cur != "" {
		out = append(out, cur)
	}

	return out
}

// workflowStep returns the step block holding lines[i]: from the nearest
// `- ` item line above it to the next line indented no deeper than that item.
func workflowStep(lines []string, i int) string {
	start := i
	for start > 0 && !strings.HasPrefix(strings.TrimLeft(lines[start], " "), "- ") {
		start--
	}

	indent := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))

	end := start + 1
	for end < len(lines) {
		trimmed := strings.TrimLeft(lines[end], " ")
		if trimmed != "" && len(lines[end])-len(trimmed) <= indent {
			break
		}

		end++
	}

	return strings.Join(lines[start:end], "\n")
}

// TestBuildRecipesStaticAndTrimmed keeps every shipped build of mro2nf and mre
// static and path-free, as the Makefile's build target explains; the e2e
// harness build is checked on its binaries by TestBinariesStaticAndPathFree.
func TestBuildRecipesStaticAndTrimmed(t *testing.T) {
	recipes := []struct {
		file, cgoOff string
		step         bool // CGO_ENABLED is set in the workflow step's env
	}{
		{file: "Makefile", cgoOff: "CGO_ENABLED=0"},
		{file: "deploy/runtime/Dockerfile", cgoOff: "CGO_ENABLED=0"},
		{file: ".github/workflows/release.yml", cgoOff: `CGO_ENABLED: "0"`, step: true},
	}

	for _, r := range recipes {
		raw, err := os.ReadFile("../../" + r.file)
		if err != nil {
			t.Fatalf("read %s: %v", r.file, err)
		}

		lines := logicalLines(string(raw))
		builds := 0

		for i, line := range lines {
			if !strings.Contains(line, "go build ") {
				continue
			}

			builds++

			scope := line
			if r.step {
				scope = workflowStep(lines, i)
			}

			if !strings.Contains(line, "-trimpath") {
				t.Errorf("%s: %q lacks -trimpath", r.file, strings.TrimSpace(line))
			}

			if !strings.Contains(scope, r.cgoOff) {
				t.Errorf("%s: %q does not build with %s", r.file, strings.TrimSpace(line), r.cgoOff)
			}
		}

		if builds == 0 {
			t.Errorf("%s: no go build recipe found", r.file)
		}
	}
}
