package emit

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

var traceFieldsLine = regexp.MustCompile(`(?m)^\s*fields\s*=\s*'([^']*)'$`)

// TestConfigEnablesTrace requires the local and AWS Batch configs to trace
// every task by default with the fields a failure is diagnosed from, keeping
// Nextflow's defaults, which the e2e harness and bench read by name.
func TestConfigEnablesTrace(t *testing.T) {
	for _, target := range []Target{TargetLocal, TargetAWSBatch} {
		cfg, err := configFile("img", target)
		if err != nil {
			t.Fatalf("render %s config: %v", target, err)
		}

		if !strings.Contains(cfg, "trace {\n    enabled = true\n") {
			t.Errorf("%s config does not enable the trace:\n%s", target, cfg)

			continue
		}

		m := traceFieldsLine.FindStringSubmatch(cfg)
		if m == nil {
			t.Errorf("%s config sets no trace fields", target)

			continue
		}

		fields := strings.Split(m[1], ",")
		for _, want := range []string{"hash", "name", "status", "exit", "attempt", "realtime", "peak_rss", "rchar", "wchar", "workdir"} {
			if !slices.Contains(fields, want) {
				t.Errorf("%s trace fields %q lack %q", target, fields, want)
			}
		}
	}
}
