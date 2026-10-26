package job

import (
	"errors"
	"fmt"
	"math"

	"github.com/martian-lang/martian/martian/core"
)

const (
	centiPerCore    = 100
	mbPerGB         = 1024
	extraVmemPerJob = 3
)

// Settings are the settings of mrp's jobmanagers/config.json
// (core.JobManagerSettings) a job is granted and run by: an unset thread or
// memory request gets one thread and 1 GB, an unset vmem request the memory
// plus 3 GB, and each thread variable the job's threads
// (core/jobmanager.go:65 threadEnvs).
func Settings() core.JobManagerSettings {
	return core.JobManagerSettings{
		ThreadEnvs: []string{
			"GOMAXPROCS", "MKL_NUM_THREADS", "NUMEXPR_MAX_THREADS", "NUMEXPR_NUM_THREADS",
			"OMP_NUM_THREADS", "OMP_THREAD_LIMIT", "POLARS_MAX_THREADS", "ROOT_MAX_THREADS",
			"TF_NUM_THREADS",
		},
		ThreadsPerJob: 1,
		MemGBPerJob:   1,
		ExtraVmemGB:   extraVmemPerJob,
	}
}

// ErrPool is a local pool without both cores and memory.
var ErrPool = errors.New("a local pool needs at least one core and 1 GB")

// Pool is the capacity mrp's local job manager resolves a job against, its
// --localcores and --localmem (core/jobmanager_local.go:88 setMaxCores,
// :108 setMaxMem). The zero Pool resolves a job as mrp's cluster job
// managers do.
type Pool struct {
	Cores int
	MemGB int
}

// Check refuses a pool with only one of cores and memory.
func (p Pool) Check() error {
	if p == (Pool{}) || p.Cores > 0 && p.MemGB > 0 {
		return nil
	}

	return fmt.Errorf("%w: %d cores, %d GB", ErrPool, p.Cores, p.MemGB)
}

// Resources resolves the resources of st's job in phase as core's
// getJobReqs does (core/stage.go:1494): the stage's using() with the def's
// non-zero fields over it, then the run's overrides of the node's phase
// (core/override.go:159 ApplyResourceOverrides), granted by the local job
// manager from the run's pool or else by a cluster job manager.
func (r Runtime) Resources(st Stage, phase string, def *core.JobResources) core.JobResources {
	res := request(st.Using, def)
	r.Overrides.ApplyResourceOverrides(r.fqname(st.Fqid), stageType(phase), &res)

	if r.Local == (Pool{}) {
		return clusterGrant(res)
	}

	return r.Local.grant(res)
}

// request is using with def's non-zero fields over it.
func request(using, def *core.JobResources) core.JobResources {
	var res core.JobResources
	if using != nil {
		res = *using
	}

	if def != nil {
		res.Threads = coalesce(def.Threads, res.Threads)
		res.MemGB = coalesce(def.MemGB, res.MemGB)
		res.VMemGB = coalesce(def.VMemGB, res.VMemGB)

		if def.Special != "" {
			res.Special = def.Special
		}
	}

	return res
}

// grant is res as mrp's local job manager grants it from p
// (core/jobmanager_local.go:375 GetSystemReqs): whole centicores and MiB,
// an adaptive (negative) request at least the whole pool, and every request
// capped at the pool, but vmem, which mrp caps only under --localvmem. mrp
// grants an adaptive memory request the memory free when the job starts,
// within the pool; the task is sized to the pool, so it is granted that.
func (p Pool) grant(res core.JobResources) core.JobResources {
	s := Settings()
	poolMB := int64(p.MemGB) * mbPerGB

	memMB := units(res.MemGB, mbPerGB)

	switch {
	case memMB == 0:
		memMB = int64(s.MemGBPerJob) * mbPerGB
	case memMB < 0:
		memMB = max(-memMB, poolMB)
	}

	vmemMB := units(res.VMemGB, mbPerGB)
	if vmemMB == 0 {
		vmemMB = memMB + int64(s.ExtraVmemGB)*mbPerGB
	}

	memMB = min(memMB, poolMB)
	if vmemMB > 0 && vmemMB < memMB {
		vmemMB = memMB
	}

	res.Threads = p.threads(res.Threads)
	res.MemGB = float64(memMB) / mbPerGB
	res.VMemGB = float64(vmemMB) / mbPerGB

	return res
}

// threads is the threads the local job manager grants a request of t from
// p: the whole pool for an adaptive request, and at most the pool.
func (p Pool) threads(t float64) float64 {
	centi := units(t, centiPerCore)

	switch {
	case centi == 0:
		return float64(Settings().ThreadsPerJob)
	case centi < 0 || centi > int64(p.Cores)*centiPerCore:
		return float64(p.Cores)
	default:
		return float64(centi) / centiPerCore
	}
}

// units is v in whole units of 1/per, rounded away from zero, as the local
// job manager counts a request.
func units(v float64, per int64) int64 {
	if v < 0 {
		return int64(math.Floor(v * float64(per)))
	}

	return int64(math.Ceil(v * float64(per)))
}

// clusterGrant is res as mrp's cluster job managers grant it
// (core/jobmanager_remote.go:94 GetSystemReqs), under a job template that
// takes a thread count, as every template Martian ships does, and no
// --mempercore: an adaptive request at its magnitude, whole threads, and no
// cap.
func clusterGrant(res core.JobResources) core.JobResources {
	s := Settings()

	res.Threads = math.Ceil(math.Abs(res.Threads))
	if res.Threads == 0 {
		res.Threads = float64(s.ThreadsPerJob)
	}

	res.MemGB = math.Abs(res.MemGB)
	if res.MemGB == 0 {
		res.MemGB = float64(s.MemGBPerJob)
	}

	if res.VMemGB < 1 {
		res.VMemGB = res.MemGB + float64(s.ExtraVmemGB)
	}

	return res
}

func coalesce(a, b float64) float64 {
	if a != 0 {
		return a
	}

	return b
}
