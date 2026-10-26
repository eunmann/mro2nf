package job

import (
	"errors"
	"testing"

	"github.com/martian-lang/martian/martian/core"
)

type grantCase struct {
	name       string
	using, def *core.JobResources
	want       core.JobResources
}

func checkGrants(t *testing.T, r Runtime, cases []grantCase) {
	t.Helper()

	for _, tc := range cases {
		if got := r.Resources(Stage{Using: tc.using}, PhaseMain, tc.def); got != tc.want {
			t.Errorf("%s: Resources = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestLocalGrant: from a pool, a job's request, the using() request with
// the def's over it, is granted as mrp's local job manager grants it
// (core/jobmanager_local.go:375 GetSystemReqs): whole centicores and MiB,
// one thread and 1 GB unset, an adaptive request at least the pool, every
// request at most the pool but vmem, vmem the uncapped memory plus 3 GB
// unset, never below the memory, and an adaptive vmem as asked. The mrpPool
// cases are the _jobinfo mrp wrote for testdata/res_pool under
// --localcores=2 --localmem=4.
func TestLocalGrant(t *testing.T) {
	checkGrants(t, Runtime{Local: mrpPool()}, []grantCase{
		{"defaults", nil, nil, core.JobResources{Threads: 1, MemGB: 1, VMemGB: 4}},
		{"fractions", &core.JobResources{Threads: 0.333, MemGB: 0.3}, nil, core.JobResources{Threads: 0.34, MemGB: 308.0 / 1024, VMemGB: (308.0 + 3072) / 1024}},
		{"def over using", &core.JobResources{Threads: 4, MemGB: 8, Special: "a"}, &core.JobResources{MemGB: 2, Special: "b"}, core.JobResources{Threads: 2, MemGB: 2, VMemGB: 5, Special: "b"}},
		{"adaptive within the pool", &core.JobResources{Threads: -1, MemGB: -1}, nil, core.JobResources{Threads: 2, MemGB: 4, VMemGB: 7}},
		{"adaptive beyond the pool", &core.JobResources{Threads: -1, MemGB: -6}, nil, core.JobResources{Threads: 2, MemGB: 4, VMemGB: 9}},
		{"adaptive chunk", nil, &core.JobResources{Threads: -3, MemGB: -5}, core.JobResources{Threads: 2, MemGB: 4, VMemGB: 8}},
		{"oversized", &core.JobResources{Threads: 64, MemGB: 200}, nil, core.JobResources{Threads: 2, MemGB: 4, VMemGB: 203}},
		{"oversized fractions", nil, &core.JobResources{Threads: 3.5, MemGB: 4.5}, core.JobResources{Threads: 2, MemGB: 4, VMemGB: 7.5}},
		{"vmem below mem", &core.JobResources{MemGB: 3, VMemGB: 1}, nil, core.JobResources{Threads: 1, MemGB: 3, VMemGB: 3}},
		{"vmem uncapped", &core.JobResources{MemGB: 8, VMemGB: 64}, nil, core.JobResources{Threads: 1, MemGB: 4, VMemGB: 64}},
		{"adaptive vmem", nil, &core.JobResources{Threads: -1, MemGB: -8, VMemGB: -2}, core.JobResources{Threads: 2, MemGB: 4, VMemGB: -2}},
	})

	// #545's host: 20 cores and 15.3 GB, mrp's default pool 20 cores and
	// 13 GB, where mrp granted an adaptive request the free memory.
	checkGrants(t, Runtime{Local: Pool{Cores: 20, MemGB: 13}}, []grantCase{
		{"adaptive", &core.JobResources{Threads: -2, MemGB: -1}, nil, core.JobResources{Threads: 20, MemGB: 13, VMemGB: 16}},
		{"oversized", &core.JobResources{Threads: 64, MemGB: 200}, nil, core.JobResources{Threads: 20, MemGB: 13, VMemGB: 203}},
		{"within", &core.JobResources{Threads: 4, MemGB: 8}, nil, core.JobResources{Threads: 4, MemGB: 8, VMemGB: 11}},
	})
}

// TestClusterGrant: with no pool, a job's request is granted as mrp's
// cluster job managers grant it (core/jobmanager_remote.go:94
// GetSystemReqs): an adaptive request at its magnitude, threads rounded up
// to whole threads, memory as asked, one thread and 1 GB unset, vmem under
// 1 GB the memory plus 3 GB, and no cap.
func TestClusterGrant(t *testing.T) {
	mem := 0.3

	checkGrants(t, Runtime{}, []grantCase{
		{"defaults", nil, nil, core.JobResources{Threads: 1, MemGB: 1, VMemGB: 4}},
		{"fractions", &core.JobResources{Threads: 0.333, MemGB: mem}, nil, core.JobResources{Threads: 1, MemGB: mem, VMemGB: mem + 3}},
		{"def over using", &core.JobResources{Threads: 4, MemGB: 8, Special: "a"}, &core.JobResources{MemGB: 2, Special: "b"}, core.JobResources{Threads: 4, MemGB: 2, VMemGB: 5, Special: "b"}},
		{"adaptive", &core.JobResources{Threads: -2.5, MemGB: -6, VMemGB: -2}, nil, core.JobResources{Threads: 3, MemGB: 6, VMemGB: 9}},
		{"oversized", &core.JobResources{Threads: 64, MemGB: 200}, nil, core.JobResources{Threads: 64, MemGB: 200, VMemGB: 203}},
		{"vmem below mem", &core.JobResources{MemGB: 8, VMemGB: 2}, nil, core.JobResources{Threads: 1, MemGB: 8, VMemGB: 2}},
		{"vmem below 1 GB", &core.JobResources{MemGB: 2, VMemGB: 0.5}, nil, core.JobResources{Threads: 1, MemGB: 2, VMemGB: 5}},
	})
}

// TestPoolCheck: a pool has both cores and memory, or neither.
func TestPoolCheck(t *testing.T) {
	for _, p := range []Pool{{}, {Cores: 1, MemGB: 1}, {Cores: 20, MemGB: 13}} {
		if err := p.Check(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}

	for _, p := range []Pool{{Cores: 2}, {MemGB: 4}, {Cores: -1, MemGB: 4}} {
		if err := p.Check(); !errors.Is(err, ErrPool) {
			t.Errorf("%+v: Check = %v, want %v", p, err, ErrPool)
		}
	}
}
