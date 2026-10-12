package cgnf

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/eunmann/mro2nf/internal/job"
)

// runtimeImage is the repository of mro2nf's release runtime image
// (deploy/runtime/Dockerfile), tagged with the release version.
const runtimeImage = "ghcr.io/eunmann/mro2nf-runtime"

// In-image paths of the runtime image and the stage image built from it.
const (
	imageRoot = "/opt/mro2nf"
	imageCode = imageRoot + "/code"
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

// The tools every task runs and the root its code is found below: the
// host's for the local and grid profiles, the image's for the container
// profiles (conf/container.config).
`)

	fmt.Fprintf(&b, "params.mro2nf_mre = %s\n", gstr(p.opts.Mre))
	fmt.Fprintf(&b, "params.mro2nf_mrjob = %s\n", gstr(p.opts.Mrjob))
	fmt.Fprintf(&b, "params.mro2nf_adapters = %s\n", gstr(p.opts.Adapters))
	b.WriteString("params.mro2nf_code = ''\n")
	fmt.Fprintf(&b, "params.mro2nf_psid = %s\n", gstr(p.src.Invocation.Call))
	fmt.Fprintf(&b, "params.mro2nf_monitor = %t\n", p.opts.Monitor)
	fmt.Fprintf(&b, "params.mro2nf_confine = %t\n\n", p.opts.Confine)

	b.WriteString(`// The entry file values a run stages: a head-node path where the profile
// sees the head node's files, an s3:// URI always, and a URI of any other
// scheme only when whoever supplies the run's params is trusted to fetch it.
params.mro2nf_host_inputs = true
params.mro2nf_allow_remote_inputs = false

// The container profiles' images: the stage image ./build.sh builds from
// this project's Dockerfile, and mro2nf's runtime image, which PLAN and OUTS
// run in, as they run no stage code.
params.mro2nf_image = null
`)

	fmt.Fprintf(&b, "params.mro2nf_runtime_image = %s\n\n", gstr(runtimeImage+":"+p.opts.Version))
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
// run, as it ends mrp's.
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

    // Every task in its own container, which mounts only what the task
    // stages: --mro2nf_image names the stage image ./build.sh built.
    docker {
        process.executor = 'local'
        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() ]
        docker.enabled = true
        includeConfig 'conf/container.config'
    }

    // AWS Batch over an S3 work dir (-work-dir s3://<bucket>/work): set
    // process.queue and aws.region in a -c config, push the stage image to
    // ECR and name it with --mro2nf_image; --outdir s3://<bucket>/<prefix>
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
    // workflow's own config, with the images it pins.
    healthomics {
        includeConfig 'conf/healthomics.config'
    }
}
`

// containerConfig is conf/container.config, the container profiles' tools
// and code, which the stage image holds where the runtime image puts them,
// and each task's image: the stage image's, but the runtime image's for
// PLAN and OUTS. Outputs are confined to the task, as no other task could
// see a path outside it.
func containerConfig() string {
	return fmt.Sprintf(`// The container profiles' tools, code and images (nextflow.config).
params.mro2nf_mre = '%[1]s/mre'
params.mro2nf_mrjob = '%[1]s/mrjob'
params.mro2nf_adapters = '%[1]s/adapters'
params.mro2nf_code = '%[2]s'
params.mro2nf_confine = true

process {
    container = params.mro2nf_image

    withName: 'PLAN|OUTS' {
        container = params.mro2nf_runtime_image
    }
}
`, imageRoot, imageCode)
}

// healthomicsConfig is conf/healthomics.config: HealthOmics runs every
// task in a private-ECR container with no head-node files, manages the
// executor and the work dir, and exports only its pubdir.
const healthomicsConfig = `// The healthomics profile (nextflow.config), the workflow's own config in
// the package package.sh builds.
includeConfig 'container.config'

params.mro2nf_host_inputs = false
params.outdir = '/mnt/workflow/pubdir'
`
