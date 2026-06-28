package overrides_test

import (
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/overrides"
)

func TestConvert(t *testing.T) {
	in := []byte(`{
	  "MAIN.PHASER.ALIGN":   { "mem_gb": 8, "threads": 4, "chunk.mem_gb": 16 },
	  "MAIN.PHASER.COLLATE": { "join.mem_gb": 32, "join.threads": 2 },
	  "":                    { "mem_gb": 2 }
	}`)

	cfg, unmapped, err := overrides.Convert(in)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	for _, want := range []string{
		"process {",
		"memory = '2 GB'", // the "" key -> global default
		"withName: 'ALIGN.*' { memory = '8 GB'; cpus = 4 }",
		"withName: 'ALIGN_MAIN.*' { memory = '16 GB' }",             // chunk.* -> _MAIN
		"withName: 'COLLATE_JOIN.*' { memory = '32 GB'; cpus = 2 }", // join.* -> _JOIN
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q\n--- got ---\n%s", want, cfg)
		}
	}

	if len(unmapped) != 0 {
		t.Errorf("unexpected unmapped fields: %v", unmapped)
	}
}

// TestConvertUnmappable checks that fields with no faithful Nextflow directive
// (virtual memory, VDR volatility, profiling) are reported, not silently emitted.
func TestConvertUnmappable(t *testing.T) {
	in := []byte(`{"P.S": {"vmem_gb": 20, "force_volatile": true, "profile": "cpu"}}`)

	cfg, unmapped, err := overrides.Convert(in)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if strings.Contains(cfg, "withName") {
		t.Errorf("unmappable-only override should emit no selectors, got:\n%s", cfg)
	}
	if len(unmapped) != 3 {
		t.Errorf("want 3 unmapped fields (vmem_gb, force_volatile, profile), got %d: %v", len(unmapped), unmapped)
	}
}

func TestConvertMalformed(t *testing.T) {
	if _, _, err := overrides.Convert([]byte("not json")); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}
