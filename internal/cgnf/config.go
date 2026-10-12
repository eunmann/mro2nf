package cgnf

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/job"
)

// config renders nextflow.config: the engine floor, the tools, code and
// knobs the stage module reads, one param per entry input, the retry
// policy and the profiles a run is launched with.
func (p *project) config() string {
	var b strings.Builder

	b.WriteString(`// The path 'arity' option, eval outputs and the local profile's
// process.resourceLimits clamp need Nextflow 24.04.
manifest.nextflowVersion = '!>=24.04.0'

params.outdir = 'results'

// Martian 'special' scheduler-resource map (the MRO_JOBRESOURCES analog):
// a stage's special key to the clusterOptions of a grid executor.
params.job_resources = [:]

// The mre every task runs, a PATH command by default: the host's on the
// local and grid profiles, the image's on the container profiles. mre runs
// py and comp stages under the mrjob beside it and py stages with the
// Martian adapters it embeds; set these to run another Martian runtime's,
// such as a pipeline bundle's own.
params.mro2nf_mre = 'mre'
params.mro2nf_mrjob = null
params.mro2nf_adapters = null

// mrp's --monitor: mrjob enforces each job's memory and thread limits.
params.mro2nf_monitor = false

// Fail a stage task whose output resolves outside its work dir instead of
// copying it in, for a run under an isolating executor such as docker.
params.mro2nf_confine = false

`)

	fmt.Fprintf(&b, "params.mro2nf_psid = %s\n\n", gstr(p.src.Invocation.Call))

	b.WriteString(`// The entry file values a run stages: a head-node path where the profile
// sees the head node's files, an s3:// URI always, and a URI of any other
// scheme only when whoever supplies the run's params is trusted to fetch it.
params.mro2nf_host_inputs = true
params.mro2nf_allow_remote_inputs = false

// The image every task of a container profile runs in, which those
// profiles require: one built FROM mro2nf's runtime image that holds the
// pipeline's code at the paths it was transpiled from, or the runtime image
// itself where a -c config mounts the code there.
params.mro2nf_image = null
params.mro2nf_require_image = false

// mrp's --overrides file, over each stage's resources; PLAN reads it as
// mrp does.
params.mro2nf_overrides = null

`)

	b.WriteString("// The entry inputs, null keeping the transpiled invocation's value.\n")

	for _, id := range slices.Sorted(maps.Keys(p.src.Invocation.Args)) {
		fmt.Fprintf(&b, "params.%s = null\n", id)
	}

	b.WriteString(processConfig())
	b.WriteString(profilesConfig)

	return b.String()
}

// processConfig is the retry policy, the GPU rule and the trace.
func processConfig() string {
	return fmt.Sprintf(`
// mrp retries a job only when its error names a signal, ENOMEM or a
// scheduler failure (jobmanagers/retry.json), for which mre exits %d; a
// task the executor killed by a signal exits 128 + the signal. Any other
// failure, a stage exception or ASSERT (42) or a failed mre step, ends the
// run, as it ends mrp's. A retry requests the same cpus and memory, as mrp
// retries a job at its request without --auto-adjust-memory.
process {
    errorStrategy = { task.exitStatus in [%d, 137, 143] ? 'retry' : 'terminate' }
    maxRetries = %d

    // A job's special "gpu" or "gpu:N", as mrp resolves it (the stage's, or
    // the one its split returned for a chunk or the join), requests 1 or N
    // GPUs, beside the clusterOptions params.job_resources maps it to. A
    // process directive cannot read a task's inputs on Nextflow 24.04; a
    // config closure can.
    accelerator = { def s = meta.special ?: ''; def n = s == 'gpu' ? 1 : (s.startsWith('gpu:') && s.substring(4).isInteger() ? s.substring(4) as int : 0); n > 0 ? [request: n] : null }
}

trace {
    enabled = true
    fields  = 'task_id,hash,native_id,name,status,exit,attempt,submit,duration,realtime,%%cpu,peak_rss,peak_vmem,rchar,wchar,workdir'
}
`, job.TransientExitCode, job.TransientExitCode, job.DefaultRetries)
}

// profilesConfig are the targets one project runs on, chosen at launch
// with -profile. It comes last: a profile's settings override the ones
// above it.
const profilesConfig = `
// The local executors clamp a request to the host's cpus and memory;
// override the clamp with a -c config setting process.resourceLimits.
profiles {
    standard {
        process.executor = 'local'
        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() ]
    }
    slurm    { process.executor = 'slurm' }
    sge      { process.executor = 'sge' }
    lsf      { process.executor = 'lsf' }
    pbs      { process.executor = 'pbs' }

    // Every task in its own container of --mro2nf_image, which mounts only
    // what the task stages.
    docker {
        process.executor = 'local'
        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() ]
        docker.enabled = true
        includeConfig 'conf/container.config'
    }

    // AWS Batch over an S3 work dir (-work-dir s3://<bucket>/work): set
    // process.queue and aws.region in a -c config, push the image to ECR
    // and name it with --mro2nf_image; --outdir s3://<bucket>/<prefix>
    // publishes to S3. The submission and transfer limits keep a wide fan-out
    // from throttling the Batch and S3 APIs and re-run a Spot reclaim.
    awsbatch {
        process.executor = 'awsbatch'
        includeConfig 'conf/container.config'
        params.mro2nf_host_inputs = false
        executor.submitRateLimit = '50/1sec'
        executor.queueSize = 1000
        aws.batch.maxParallelTransfers = 5
        aws.batch.maxTransferAttempts = 3
        aws.batch.delayBetweenAttempts = '10 sec'
        aws.batch.maxSpotAttempts = 5
        aws.batch.retryMode = 'standard'
    }

    // AWS HealthOmics, which takes no -profile: package.sh makes it the
    // workflow's own config, with the image it pins.
    healthomics {
        includeConfig 'conf/healthomics.config'
    }
}
`

// containerConfig is conf/container.config, the one image every task of
// the container profiles runs in, whose PATH holds mre. Outputs are
// confined to the task, as no other task could see a path outside it.
const containerConfig = `// The container profiles' image (nextflow.config).
params.mro2nf_confine = true
params.mro2nf_require_image = true

process.container = params.mro2nf_image
`

// healthomicsConfig is conf/healthomics.config: HealthOmics runs every
// task in a private-ECR container with no head-node files, manages the
// executor and the work dir, and exports only its pubdir.
const healthomicsConfig = `// The healthomics profile (nextflow.config), the workflow's own config in
// the package package.sh builds.
includeConfig 'container.config'

params.mro2nf_host_inputs = false
params.outdir = '/mnt/workflow/pubdir'
`
