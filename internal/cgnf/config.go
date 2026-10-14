package cgnf

import (
	"fmt"
	"strings"

	"github.com/eunmann/mro2nf/internal/job"
)

// engineFloor is the oldest Nextflow release the project is tested on. A
// workflow's nextflow.config declares it, as HealthOmics reads it there to
// choose the engine a run gets.
const engineFloor = "manifest.nextflowVersion = '!>=26.04.0'\n"

// nextflowConfig is nextflow.config: the engine floor, conf/base.config and
// the profiles a run is launched with.
const nextflowConfig = `// The oldest Nextflow release this project is tested on.
` + engineFloor + `
includeConfig 'conf/base.config'
` + profilesConfig

// baseConfig renders conf/base.config, the settings every target shares:
// the tools, code and knobs the stage module reads, one param per entry
// input, the retry policy and the trace.
func (p *project) baseConfig() string {
	var b strings.Builder

	b.WriteString("// The settings every target shares: nextflow.config and the HealthOmics\n// package's config include them.\n\n")

	for _, blk := range projectParams(p.src.Invocation.Call) {
		if blk.comment != "" {
			b.WriteString(blk.comment + "\n")
		}

		for _, lp := range blk.params {
			fmt.Fprintf(&b, "params.%s = %s\n", lp.id, groovy(lp.value))
		}

		b.WriteString("\n")
	}

	b.WriteString(publishConfig)
	b.WriteString("// The entry inputs, null keeping the transpiled invocation's value.\n")

	for _, lp := range p.inputs {
		fmt.Fprintf(&b, "params.%s = null\n", lp.id)
	}

	b.WriteString(processConfig())

	return b.String()
}

// publishConfig sets where the workflow outputs are published: --outdir, a
// directory or an object-store prefix. Nextflow's own -output-dir overrides
// it. It reads params.outdir as it stands here, so a later config that sets
// params.outdir must set outputDir too, or main.nf refuses the run
// (planWiring).
const publishConfig = `// The run publishes its outputs below --outdir, unless -output-dir names
// another dir; a later config that sets params.outdir sets outputDir too.
outputDir = params.outdir

`

// processConfig is the retry policy, the GPU rule and the trace.
func processConfig() string {
	return fmt.Sprintf(`
// Only a failure that can pass on a second attempt is retried: mre exits %d
// when a job's error names a transient cause (a signal, ENOMEM, a scheduler
// failure, mrp's jobmanagers/retry.json), and an executor kill or timeout
// exits 128 + its signal, or 104, the range nf-core pipelines retry. A stage
// exception, an ASSERT (42) or a failed mre step fails again, so it ends the
// run on its first attempt. A retry requests the same cpus and memory, as
// mrp retries a job at its request without --auto-adjust-memory.
process {
    errorStrategy = { task.exitStatus in ([%d, 104] + (130..145)) ? 'retry' : 'terminate' }
    maxRetries = %d

    // A job's special "gpu" or "gpu:N", as mrp resolves it (the stage's, or
    // the one its split returned for a chunk or the join), requests 1 or N
    // GPUs, beside the clusterOptions params.job_resources maps it to.
    accelerator = { def s = meta.special ?: ''; def n = s == 'gpu' ? 1 : (s.startsWith('gpu:') && s.substring(4).isInteger() ? s.substring(4) as int : 0); n > 0 ? [request: n] : null }
}

trace {
    enabled = true
    fields  = 'task_id,hash,native_id,name,status,exit,attempt,submit,duration,realtime,%%cpu,peak_rss,peak_vmem,rchar,wchar,workdir'
}
`, job.TransientExitCode, job.TransientExitCode, job.DefaultRetries)
}

// profilesConfig are the targets one project runs on, chosen at launch
// with -profile. It comes after conf/base.config: a profile's settings
// override the ones there.
const profilesConfig = `
// The local profiles grant each job from a pool of the cpus and 90% of the
// memory the JVM sees, and clamp each task to it; a -c config setting
// process.resourceLimits sets another pool. Without a cgroup cpu quota or
// memory limit that is mrp's default --localcores and --localmem
// (core/jobmanager_local.go setMaxCores, setMaxMem); under one the JVM
// counts the quota's cpus, where mrp counts every cpu in its affinity mask,
// and mrp may take 96% of the limit.
profiles {
    standard {
        process.executor = 'local'
        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() * 0.9 ]
        params.mro2nf_local_jobs = true
    }
    slurm    { process.executor = 'slurm' }
    sge      { process.executor = 'sge' }
    lsf      { process.executor = 'lsf' }
    pbs      { process.executor = 'pbs' }

    // Every task in its own container of --mro2nf_image, which mounts only
    // what the task stages. The container runs as the invoking user so work/
    // is not root-owned; the task's shell expands $(id), and the image has no
    // home for that uid, so HOME is /tmp. containerOptions, not
    // docker.runOptions, so a -c docker.runOptions adds to the mapping.
    docker {
        process.executor = 'local'
        process.resourceLimits = [ cpus: nextflow.util.SysHelper.getAvailCpus(), memory: nextflow.util.SysHelper.getAvailMemory() * 0.9 ]
        params.mro2nf_local_jobs = true
        docker.enabled = true
        process.containerOptions = '-u $(id -u):$(id -g) -e HOME=/tmp'
        includeConfig 'conf/container.config'
    }

    // AWS Batch over an S3 work dir (-work-dir s3://<bucket>/work): set
    // process.queue and aws.region in a -c config, push the image to ECR
    // and name it with --mro2nf_image; --outdir s3://<bucket>/<prefix>
    // publishes to S3. The submission and transfer limits keep a wide fan-out
    // from throttling the Batch and S3 APIs and re-run a Spot reclaim. The
    // deploy/awsbatch-cdk instances mount the input buckets at /mnt/s3; a
    // --mro2nf_s3_mount naming another root binds that root instead. The poll
    // interval names $awsbatch because the local executor, which polls every
    // 100 ms, reads an unscoped executor.pollInterval too.
    awsbatch {
        process.executor = 'awsbatch'
        includeConfig 'conf/container.config'
        params.mro2nf_host_inputs = false
        params.mro2nf_s3_mount = '/mnt/s3'
        aws.batch.volumes = "${params.mro2nf_s3_mount}:${params.mro2nf_s3_mount}:ro"
        executor.submitRateLimit = '50/1sec'
        executor.queueSize = 1000
        executor.$awsbatch.pollInterval = '2 sec'
        aws.batch.maxParallelTransfers = 5
        aws.batch.maxTransferAttempts = 3
        aws.batch.delayBetweenAttempts = '10 sec'
        aws.batch.maxSpotAttempts = 5
        aws.batch.retryMode = 'standard'
    }

    // AWS HealthOmics, which applies standard to a run that names no
    // profile: package.sh makes conf/base.config and this profile's config
    // the workflow's own, with the image it pins and no profiles.
    healthomics {
        includeConfig 'conf/healthomics.config'
    }
}
`

// containerConfig is conf/container.config, the one image every task of
// the container profiles runs in, whose PATH holds mre. Outputs are
// confined to the task, as no other task could see a path outside it, and
// a head-node input is staged, as no task sees the host's path.
const containerConfig = `// The container profiles' image (nextflow.config).
params.mro2nf_confine = true
params.mro2nf_require_image = true
params.mro2nf_host_in_place = false

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
outputDir = params.outdir

// HealthOmics gives a task whole GiB, rounding a fractional request down, and
// refuses one under 1 GiB; rounding up keeps every task at or above mrp's
// request. The stage still sees mrp's value in _jobinfo. Only a selector
// outranks the stage modules' own memory directive.
process {
    withName: '.*' {
        memory = { meta.mem_gb == null ? null : "${Math.max(1L, Math.ceil(meta.mem_gb as double) as long)} GB" }
    }
}
`
