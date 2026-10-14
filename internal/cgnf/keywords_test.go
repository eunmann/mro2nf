package cgnf

import "testing"

// TestNextflowKeyword (#585): every name Nextflow's params block refuses
// is a keyword, and the names beside them it declares, and a config and a
// launch value set, are not. Each verdict was taken from a run of Nextflow
// 26.04.0 and 26.04.7 declaring the name in a params block, setting it to
// null in nextflow.config and printing params.<name> as a launch value sets
// it.
func TestNextflowKeyword(t *testing.T) {
	refused := []string{
		"as", "def", "assert", "catch", "else", "enum", "if", "import", "include", "instanceof", "new", "return", "throw", "try",
		"boolean", "byte", "char", "double", "float", "int", "long", "short",
		"true", "false", "null",
		"abstract", "break", "case", "class", "const", "continue", "default", "do", "extends", "final", "finally", "for", "goto",
		"implements", "interface", "native", "package", "private", "protected", "public", "static", "super", "switch",
		"synchronized", "this", "throws", "transient", "void", "volatile", "while",
		"_",
	}
	for _, name := range refused {
		if !nextflowKeyword(name) {
			t.Errorf("nextflowKeyword(%q) = false; Nextflow's params block refuses it", name)
		}
	}

	declared := []string{
		"in", "nextflow", "params", "from", "record", "process", "exec", "input", "output", "script", "shell", "stage", "stub",
		"topic", "tuple", "when", "workflow", "emit", "main", "onComplete", "onError", "publish", "take", "includeConfig",
		"permits", "sealed", "strictfp", "threadsafe", "trait", "var", "yield",
		"env", "it", "log", "task", "file", "secrets", "launchDir", "projectDir", "moduleDir", "workDir", "baseDir",
		"compiled", "disabled", "filetype", "local", "mem_gb", "vmem_gb", "preflight", "retain", "special", "split", "strict",
		"struct", "threads", "using",
		"_x", "X", "Abc", "x_1",
	}
	for _, name := range declared {
		if nextflowKeyword(name) {
			t.Errorf("nextflowKeyword(%q) = true; Nextflow's params block declares it", name)
		}
	}
}
