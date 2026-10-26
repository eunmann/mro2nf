package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eunmann/mro2nf/internal/job"
	"github.com/google/go-cmp/cmp"
)

// planResources is each node's resources in the plan in work, by fqid: the
// cpus and memory its first task is scheduled with and its own overrides.
func planResources(t *testing.T, work string) map[string]string {
	t.Helper()

	out := map[string]string{}

	for line := range strings.SplitSeq(strings.TrimSpace(readText(t, filepath.Join(work, "nodes"))), "\n") {
		f := strings.Fields(line)
		if len(f) != 8 {
			t.Fatalf("nodes line %q: want fqid, digest, off, disabled keys, cpus, memory, overrides and collapsed roots", line)
		}

		out[f[0]] = strings.Join(f[4:7], " ")
	}

	return out
}

// TestPlanResolvesOverrides: mre plan schedules each node's first task, its
// split or its one main, with the cpus and memory its job gets under the
// run's mrp --overrides file, as mrp resolves them for overrides_resources,
// and gives each node the overrides file of its own its jobs resolve
// theirs under; without one, a node's using() alone.
func TestPlanResolvesOverrides(t *testing.T) {
	fx := fixtureDir(t, "overrides_resources")

	cases := []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{"none", nil, map[string]string{
			"OVR.INNER.PREP": "1 1 -", "OVR.INNER.SQUARES": "1 1 -", "OVR.REPORT": "1 1 -", "OVR.OTHER": "1 1 -", "OVR.EACH": "1 1 -",
		}},
		{"overrides.json", []string{"-overrides", filepath.Join(fx, "overrides.json"), "-psid", "mrp"}, map[string]string{
			"OVR.INNER.PREP":    `1 1.5 {"OVR.INNER.PREP":{"chunk.threads":0.5,"chunk.mem_gb":1.5}}`,
			"OVR.INNER.SQUARES": `1 2.5 {"OVR.INNER.SQUARES":{"join.threads":1.5,"join.vmem_gb":7,"chunk.threads":1.25,"chunk.mem_gb":3,"split.mem_gb":2.5,"split.vmem_gb":6}}`,
			"OVR.REPORT":        `1 1.5 {"OVR.REPORT":{"chunk.mem_gb":1.5}}`,
			"OVR.OTHER":         `2 2.25 {"OVR.OTHER":{"chunk.threads":2,"chunk.mem_gb":2.25,"chunk.vmem_gb":5}}`,
			"OVR.EACH":          `1 0.75 {"OVR.EACH":{"chunk.mem_gb":0.75}}`,
		}},
	}

	for _, tc := range cases {
		work, err := planEntry(t, fx, "{}", tc.flags...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if d := cmp.Diff(tc.want, planResources(t, work)); d != "" {
			t.Errorf("%s: node resources (-want +got):\n%s", tc.name, d)
		}
	}
}

// TestPlanGrantsFromThePool: mre plan schedules each node's first task with
// what its job is granted: from a local pool, an adaptive or oversized
// request at most the pool, as mrp's local job manager grants res_pool's
// jobs under --localcores=2 --localmem=4; without one, as mrp's cluster job
// managers grant it, at its magnitude and uncapped. A pool needs both cores
// and memory.
func TestPlanGrantsFromThePool(t *testing.T) {
	fx := fixtureDir(t, "res_pool")

	cases := []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{"local", []string{"-local-cores", "2", "-local-mem-gb", "4"}, map[string]string{
			"RES_POOL.ADAPT": "2 4 -", "RES_POOL.BIG": "2 4 -", "RES_POOL.CHUNKED": "2 4 -",
		}},
		{"cluster", nil, map[string]string{
			"RES_POOL.ADAPT": "1 6 -", "RES_POOL.BIG": "64 200 -", "RES_POOL.CHUNKED": "3 5 -",
		}},
	}

	for _, tc := range cases {
		work, err := planEntry(t, fx, "{}", tc.flags...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if d := cmp.Diff(tc.want, planResources(t, work)); d != "" {
			t.Errorf("%s: node resources (-want +got):\n%s", tc.name, d)
		}
	}

	if _, err := planEntry(t, fx, "{}", "-local-cores", "2"); !errors.Is(err, job.ErrPool) {
		t.Errorf("plan with cores but no memory: %v, want %v", err, job.ErrPool)
	}
}

// TestPlanRefusesOverridesMrpRefuses: a key mrp's overrides reader does not
// know, such as a phaseless mem_gb, or an overrides file that does not
// exist, fails the run at its start, as mrp refuses to start
// (cmd/mrp/configure.go:367).
func TestPlanRefusesOverridesMrpRefuses(t *testing.T) {
	fx := fixtureDir(t, "overrides_resources")
	bad := writeTestFile(t, filepath.Join(t.TempDir(), "overrides.json"), `{"OVR.REPORT": {"mem_gb": 2}}`)

	for path, want := range map[string]string{
		bad: `unknown field "mem_gb"`,
		filepath.Join(t.TempDir(), "missing.json"): "no such file",
	} {
		_, err := planEntry(t, fx, "{}", "-overrides", path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("plan with overrides %s: %v, want an error naming %q", path, err, want)
		}
	}
}

// TestPlanRefusesPsidMrpRefuses: a psid mrp refuses (cmd/mrp/main.go:494
// util.ValidateID), such as one with a '.' that would shift the prefix
// Martian strips from every fqname an override matches, fails the run at
// its start.
func TestPlanRefusesPsidMrpRefuses(t *testing.T) {
	fx := fixtureDir(t, "overrides_resources")

	for _, psid := range []string{"sample.1", ""} {
		_, err := planEntry(t, fx, "{}", "-overrides", filepath.Join(fx, "overrides.json"), "-psid", psid)
		if err == nil || !strings.Contains(err.Error(), "Invalid name") {
			t.Errorf("plan with psid %q: %v, want mrp's invalid name error", psid, err)
		}
	}
}
