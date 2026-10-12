package job

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/martian-lang/martian/martian/core"
)

// stageType is mrp's stage type of phase, the phase an overrides key names
// (core/runtime.go:28): a main job is a chunk, a stage that does not split
// included (core/stage.go:1646 runChunk).
func stageType(phase string) string {
	if phase == PhaseMain {
		return core.STAGE_TYPE_CHUNK
	}

	return phase
}

// Phases are the phases of st's jobs: a split stage's split, chunks and
// join, any other stage's one main, as mrp runs no split or join job of a
// stage that does not split (core/stage.go:1080 doSplit).
func (st Stage) Phases() []string {
	if st.Split {
		return []string{PhaseSplit, PhaseMain, PhaseJoin}
	}

	return []string{PhaseMain}
}

// Override is an overrides file of one key, st's fqid, that sets of each
// phase st runs each resource r.Overrides sets for it, at the nearest key
// on its fqname by mrp's own walk (core/override.go:159
// ApplyResourceOverrides), so st's jobs resolve the same resources under
// it. It is nil when none is set, so it changes only with what applies to
// st's jobs.
func (r Runtime) Override(st Stage) ([]byte, error) {
	so, set := new(core.StageOverride), false

	for _, phase := range st.Phases() {
		// A JSON number is never NaN, so a field left NaN is one no key sets.
		res := core.JobResources{Threads: math.NaN(), MemGB: math.NaN(), VMemGB: math.NaN()}
		r.Overrides.ApplyResourceOverrides(r.fqname(st.Fqid), stageType(phase), &res)

		threads, mem, vmem := phaseFields(so, phase)
		for _, f := range []struct {
			dst **float64
			v   float64
		}{{threads, res.Threads}, {mem, res.MemGB}, {vmem, res.VMemGB}} {
			if !math.IsNaN(f.v) {
				v := f.v
				*f.dst, set = &v, true
			}
		}
	}

	if !set {
		return nil, nil
	}

	raw, err := json.Marshal(map[string]*core.StageOverride{st.Fqid: so})
	if err != nil {
		return nil, fmt.Errorf("encode overrides of %s: %w", st.Fqid, err)
	}

	return raw, nil
}

// phaseFields are so's threads, mem_gb and vmem_gb of phase.
func phaseFields(so *core.StageOverride, phase string) (**float64, **float64, **float64) {
	switch phase {
	case PhaseSplit:
		return &so.SplitThreads, &so.SplitMem, &so.SplitVMem
	case PhaseJoin:
		return &so.JoinThreads, &so.JoinMem, &so.JoinVMem
	default:
		return &so.ChunkThreads, &so.ChunkMem, &so.ChunkVMem
	}
}

// CPUs is the whole cpus a task running a job that holds res is scheduled
// with: its threads, rounded up.
func CPUs(res core.JobResources) int {
	return int(math.Max(1, math.Ceil(res.Threads)))
}
