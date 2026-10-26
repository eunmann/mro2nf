package main

import (
	"path/filepath"
	"testing"
)

// TestSubcommandsRefuseBadInputs: every subcommand fails, before it writes
// anything, on a flag it does not define, an invocation that does not exist,
// and a stage bundle or record it cannot read.
func TestSubcommandsRefuseBadInputs(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "struct_proj")
	mro := filepath.Join(dir, "pipeline.mro")
	absent := filepath.Join(t.TempDir(), "absent")

	stage := func(argv ...string) []string {
		return append([]string{"stage", "-entry", mro, "-args", filepath.Join(t.TempDir(), "args"), "-work", t.TempDir()}, argv...)
	}

	cases := map[string][]string{
		"outs flag":         {"outs", "-bogus"},
		"outs entry":        {"outs", "-entry", absent},
		"outs staged":       {"outs", "-entry", mro, "-in", absent},
		"stage flag":        {"stage", "-bogus"},
		"stage entry":       {"stage", "-entry", absent, "-phase", "main"},
		"stage call":        stage("-call", writeTestFile(t, filepath.Join(t.TempDir(), "call"), "not base64!"), "-phase", "main", "-node", "PP.SUMX"),
		"stage call file":   stage("-call", absent, "-phase", "main", "-node", "PP.SUMX"),
		"stage fork record": stage("-phase", "main", "-node", "PP.MAKE", "-fork", "not base64!"),
		"stage node":        stage("-phase", "split", "-node", "PP.NONE"),
		"stage args":        {"stage", "-entry", mro, "-phase", "join", "-node", "PP.SUMX", "-args", absent},
		"plan flag":         {"plan", "-bogus"},
		"plan entry":        {"plan", "-build", testBuild(), "-entry", absent},
	}

	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(t.Context(), argv); err == nil {
				t.Errorf("mre %v succeeded", argv)
			}
		})
	}
}
