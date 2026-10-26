package buildinfo

import (
	"runtime/debug"
	"testing"
)

// TestChoose checks an unstamped build takes the module version Go stamps,
// so builds of different commits get different names, which key a task's
// -resume (#449) and which PLAN matches mre against (#626), and stays dev
// only when Go stamps none.
func TestChoose(t *testing.T) {
	const pseudo = "v0.5.1-0.20261011171927-bab46362db39+dirty"

	withMain := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/eunmann/mro2nf", Version: v}}
	}

	for _, tc := range []struct {
		name  string
		stamp string
		info  *debug.BuildInfo
		want  string
	}{
		{"ldflags_stamp_wins", "v1.2.3", withMain(pseudo), "v1.2.3"},
		{"module_version", "dev", withMain(pseudo), pseudo},
		{"devel", "dev", withMain("(devel)"), "dev"},
		{"empty_module_version", "dev", withMain(""), "dev"},
		{"no_build_info", "dev", nil, "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := choose(tc.stamp, tc.info); got != tc.want {
				t.Errorf("choose(%q) = %q, want %q", tc.stamp, got, tc.want)
			}
		})
	}
}
