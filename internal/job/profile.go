package job

import (
	"github.com/martian-lang/martian/martian/core"
)

// unsetProfile is a profile mode no overrides key sets: JSON decoding
// replaces invalid UTF-8 in a string, so no mode an overrides file sets
// holds it.
const unsetProfile core.ProfileMode = "\xff"

// stagePID is the profile arg mrjob expands to the stage's pid
// (core/profile_mode.go:124).
const stagePID = "${STAGE_PID}"

// Profiles are the profiles of mrp's jobmanagers/config.json at the Martian
// commit go.mod pins, which mrp reads beside its binary
// (core/jobmanager.go:122 getJobConfig): each mode's config, which a job's
// _jobinfo records for mrjob, which sets its env and runs its cmd, and its
// adapter mode for the stage's adapter.
func Profiles() map[core.ProfileMode]*core.ProfileConfig {
	return map[core.ProfileMode]*core.ProfileConfig{
		core.CpuProfile:  {Adapter: core.CpuProfile},
		core.LineProfile: {Adapter: core.LineProfile},
		core.MemProfile: {
			Adapter: core.MemProfile,
			Env: map[string]string{
				"MALLOC_CONF": "prof:true,prof_prefix:${RAW_PERF_DEST}.jeprof,lg_prof_sample:21",
				"HEAPPROFILE": "${RAW_PERF_DEST}.tcmalloc.pprof",
			},
		},
		core.PerfRecordProfile: {
			Command: "perf",
			Args: []string{
				"record", "-g", "-F", "${MRO_PERF_FREQ}", "-o", "${RAW_PERF_DEST}", "-e", "${MRO_PERF_EVENTS}",
				"-p", stagePID, "sleep", "${MRO_PERF_DURATION}",
			},
			Defaults: map[string]string{"MRO_PERF_FREQ": "200", "MRO_PERF_EVENTS": "task-clock", "MRO_PERF_DURATION": "2400"},
		},
		"pyflame": {
			Command: "pyflame",
			Args:    []string{"-s", "-1", "-o", "${PROFILE_DEST}", "-H", "${PROFILE_DEST}.html", stagePID},
		},
		"pyspy": {
			Command: "py-spy",
			Args: []string{
				"record", "--pid", stagePID, "--output", "${PROFILE_DEST}", "--rate", "${MRO_PERF_FREQ}",
				"--duration", "${MRO_PERF_DURATION}", "--format", "raw", "--native", "--subprocesses",
			},
			Defaults: map[string]string{"MRO_PERF_FREQ": "100", "MRO_PERF_DURATION": "4800"},
		},
	}
}

// profile sets info's profile_mode and profile_config as mrp's runJob does
// (core/node.go:1177-1193): the mode is the one the nearest overrides key
// on the job's node sets for its phase, else the run's, disable
// (core/node.go:1099 getProfileMode, core/runtime.go:185); its config is
// the profile of that name, or of the run's mode for an empty one
// (core/runtime.go:651 ProfileConfig), none for disable or a mode no
// profile names, and a config's adapter mode replaces the mode.
func (r Runtime) profile(j *job, info *core.JobInfo) {
	run := core.DisableProfile

	info.ProfileMode = r.Overrides.GetProfile(j.node, stageType(j.shell), run)

	mode := info.ProfileMode
	if mode == "" {
		mode = run
	}

	if mode != core.DisableProfile {
		info.ProfileConfig = Profiles()[mode]
	}

	if info.ProfileConfig != nil && info.ProfileConfig.Adapter != "" {
		info.ProfileMode = info.ProfileConfig.Adapter
	}
}
