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

// TestRuntimeBuildGoIsToolchain keeps the runtime image's mre on the Go that
// go.mod's toolchain line names (the golang image never switches toolchains),
// so a Dependabot golang bump also has to move the toolchain line.
func TestRuntimeBuildGoIsToolchain(t *testing.T) {
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	toolchain := ""

	for line := range strings.Lines(string(mod)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "toolchain go"); ok {
			toolchain = v
		}
	}

	if toolchain == "" {
		t.Fatal("go.mod has no toolchain line")
	}

	raw, err := os.ReadFile("../../deploy/runtime/Dockerfile")
	if err != nil {
		t.Fatalf("read runtime Dockerfile: %v", err)
	}

	want, found := "golang:"+toolchain, false

	for _, line := range logicalLines(string(raw)) {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "FROM" {
			continue
		}

		for _, ref := range f[1:] {
			if !strings.HasPrefix(ref, "golang:") {
				continue
			}

			found = true

			if tag, _, _ := strings.Cut(ref, "@"); tag != want {
				t.Errorf("runtime build stage FROM %s, want %s (go.mod toolchain)", tag, want)
			}
		}
	}

	if !found {
		t.Error("deploy/runtime/Dockerfile has no golang build stage")
	}
}

// TestGolangciLintPinsAgree keeps make lint, the pre-commit hook and CI on
// one golangci-lint: a release older than the go.mod toolchain fails to
// typecheck, so a toolchain bump moves every pin together.
func TestGolangciLintPinsAgree(t *testing.T) {
	pins := map[string]string{}

	for _, file := range []string{"Makefile", ".claude/hooks/pre-commit-lint.sh"} {
		raw, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		_, after, ok := strings.Cut(string(raw), "golangci-lint@")
		if !ok {
			t.Fatalf("%s runs no pinned golangci-lint", file)
		}

		pins[file] = strings.Fields(after)[0]
	}

	const workflow = ".github/workflows/pr-validation.yml"

	raw, err := os.ReadFile("../../" + workflow)
	if err != nil {
		t.Fatalf("read %s: %v", workflow, err)
	}

	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "golangci/golangci-lint-action@") {
			continue
		}

		for l := range strings.Lines(workflowStep(lines, i)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "version:"); ok {
				pins[workflow] = strings.TrimSpace(v)
			}
		}
	}

	if pins[workflow] == "" {
		t.Fatalf("%s pins no golangci-lint version", workflow)
	}

	for file, v := range pins {
		if v != pins["Makefile"] {
			t.Errorf("%s pins golangci-lint %s, the Makefile %s", file, v, pins["Makefile"])
		}
	}
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
