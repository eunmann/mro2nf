package main

import (
	"path/filepath"
	"testing"
)

// TestSubcommandsRefuseBadInputs: every subcommand fails, before it writes
// anything, on a flag it does not define, an invocation or type manifest
// that does not exist, and a stage bundle or record it cannot read.
func TestSubcommandsRefuseBadInputs(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "struct_proj")
	mro := filepath.Join(dir, "pipeline.mro")
	absent := filepath.Join(t.TempDir(), "absent")

	cases := map[string][]string{
		"args flag":        {"args", "-bogus"},
		"args entry":       {"args", "-entry", absent},
		"args call":        {"args", "-entry", mro, "-call", writeTestFile(t, filepath.Join(t.TempDir(), "call"), "not base64!")},
		"args call file":   {"args", "-entry", mro, "-call", absent},
		"args types":       {"args", "-entry", mro, "-types", absent},
		"args fork record": {"args", "-entry", mro, "-node", "PP.MAKE", "-fork", "not base64!"},
		"args node":        {"args", "-entry", mro, "-node", "PP.NONE", "-o", filepath.Join(t.TempDir(), "args")},
		"post flag":        {"post", "-bogus"},
		"post entry":       {"post", "-entry", absent},
		"post args":        {"post", "-entry", mro, "-args", absent, "-outs", absent},
		"outs flag":        {"outs", "-bogus"},
		"outs entry":       {"outs", "-entry", absent},
		"outs staged":      {"outs", "-entry", mro, "-in", absent},
		"run flag":         {"run", "-bogus"},
		"run entry":        {"run", "-entry", absent, "-phase", "main"},
		"run args":         {"run", "-entry", mro, "-phase", "main", "-args", absent},
		"plan flag":        {"plan", "-bogus"},
		"plan entry":       {"plan", "-entry", absent},
	}

	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(t.Context(), argv); err == nil {
				t.Errorf("mre %v succeeded", argv)
			}
		})
	}
}
