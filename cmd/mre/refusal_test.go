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
		"args flag":            {"args", "-bogus"},
		"args mro":             {"args", "-mro", absent},
		"args types":           {"args", "-mro", mro, "-types", absent},
		"args fork record":     {"args", "-mro", mro, "-node", "PP.MAKE", "-fork", "not base64!"},
		"args node":            {"args", "-mro", mro, "-node", "PP.NONE", "-o", filepath.Join(t.TempDir(), "args")},
		"post flag":            {"post", "-bogus"},
		"post mro":             {"post", "-mro", absent},
		"post args":            {"post", "-mro", mro, "-args", absent, "-outs", absent},
		"outs flag":            {"outs", "-bogus"},
		"outs mro":             {"outs", "-mro", absent},
		"outs staged":          {"outs", "-mro", mro, "-in", absent},
		"run flag":             {"run", "-bogus"},
		"run mro":              {"run", "-mro", absent, "-phase", "main"},
		"run args":             {"run", "-mro", mro, "-phase", "main", "-args", absent},
		"plan flag":            {"plan", "-bogus"},
		"plan source":          {"plan", "-source", absent},
		"publish-layout flag":  {"publish-layout", "-bogus"},
		"publish-layout types": {"publish-layout", "-types", absent, "-sidecar", absent},
	}

	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(t.Context(), argv); err == nil {
				t.Errorf("mre %v succeeded", argv)
			}
		})
	}
}
