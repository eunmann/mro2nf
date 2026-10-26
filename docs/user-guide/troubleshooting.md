# Troubleshooting

## When a stage fails

Nextflow prints the failed process, its exit status, the error the stage or
`mre` wrote, and the task's work directory, then exits with status 1. A failed
run publishes nothing to `results/`.

| Task exit | Meaning | Retried |
|---|---|---|
| 42 | the stage failed an `ASSERT` (`martian.exit`) | no |
| 75 | a failure `mrp` retries: a signal, `ENOMEM`, a scheduler failure | up to twice |
| 104, 130–145 | the executor killed the task with a signal or stopped it at a time limit (137 is an out-of-memory kill) | up to twice |
| 1 | a stage exception, or a failed `mre` step | no |

A retried attempt that failed still counts as a failed task in the run's
summary, even when the run succeeds.

## Inside a task's work directory

Each stage task runs one job, one phase of one stage instance, so its work
directory holds that job's metadata directory:

```
work/ab/cdef…/
├── .command.sh      the task's script
├── .command.out     its stdout
├── .command.err     its stderr
├── chnk0/           the job's metadata: split/ for a split, chnk<N>/ for a
│                    main (chnk0/ for a stage that does not split), join/
│                    for a join
└── journal/         the job journal files mrjob writes
```

The metadata directory holds `_args`, `_jobinfo`, `_outs`, `_stdout`,
`_stderr`, `_log`, and `_errors` for an exception or `_assert` for an `ASSERT`:
the same files `mrp` writes for the job.

### On AWS Batch

On `awsbatch` the work directory is on S3, and a task runs in a directory on
its instance. Nextflow copies a task's outputs to S3 only when the task
succeeds, so a failed task's S3 work directory holds Nextflow's own files and
not the job's metadata directory:

- `.command.sh` and `.command.run`, the task's script and its wrapper;
- `.command.out` and `.command.err`, its stdout and stderr, and `.command.log`,
  both together, which is also the Batch job's CloudWatch log stream;
- `.command.trace` and `.exitcode`.

`mre` copies the job's stdout and stderr, its `_stdout` and `_stderr`, to the
task's, and its error names the job and carries the text of `_errors` or
`_assert`, so `.command.err` and `.command.log` hold them. `_args`, `_jobinfo`,
`_outs` and `_log` are not kept. To read them, run the pipeline with the same
inputs on the `docker` profile, whose work directory keeps them.

## Error messages

### The pipeline and its code

- **`project transpiled by <build>, mre is <build>`**: PLAN runs a project only
  under the `mre` of the `mro2nf` build that transpiled it. Put that build's
  `mre` on the tasks' `PATH` (or name it with `--mro2nf_mre`), use the runtime
  image of its tag, or transpile the pipeline again with the `mro2nf` of the
  `mre` you run. A local runtime image takes the build with `--build-arg
  VERSION` ([Container images](container-images.md#the-runtime-image)).
- **`the node compiles to another spec than the run's plan holds;
  re-transpile the pipeline, and rebuild its image, from the same sources`**:
  the `.mro` changed after the transpile, or the image holds other sources
  than the ones transpiled. Transpile again, and rebuild the image.
- **`mre: load call graph: read invocation: open <path>: no such file or
  directory`**: the task cannot see the pipeline's code at the path it was
  transpiled from. In a container, `COPY` the code to that path in the image
  ([Container images](container-images.md)).
- **`<file>:<line>:<column>: number literal out of range: an int must fit
  int64, a float float64 and a threads, mem_gb or vmem_gb value float32:
  <literal>`**: `mro2nf`
  refuses an int literal outside int64, a float literal beyond float64, or a
  stage's `threads`, `mem_gb` or `vmem_gb` beyond float32 in the `.mro` or a
  file it includes, where `mrp` panics on it.

### Launch values

- **`entry input "<name>": number literal out of range: an int must fit
  int64, a float float64 and a threads, mem_gb or vmem_gb value float32:
  <literal>`**: PLAN refuses a
  launch value holding an int outside int64 or a float beyond float64 at any
  depth, as `mrp` refuses the literal.
- **`entry override of a non-scalar type cannot be a string`**: pass the
  array, map or struct in a `-params-file`
  ([Parameters](parameters.md#how-to-pass-a-value)).
- **`entry override of a split input must be {"split": [...]} or {"split":
  {...}}`**: the input forks the top-level `map call`; pass its value in
  Martian's split form in a `-params-file`.

### File inputs

- **`entry input '<name>': the path contains '<c>', which Nextflow's task
  stage-in script leaves unquoted`**: Nextflow cannot stage a file whose name
  contains `*`, `?`, `[`, `]`, `{`, `}`, `$` or a control character. Rename the
  file, or, on the `standard` and grid profiles, pass it as an absolute path
  rather than a `file:` URI
  ([Files and paths](files-and-paths.md#file-names-nextflow-cannot-stage)).
- **`entry input '<name>': a head-node path is not staged on this profile;
  pass an s3:// URI`**: the `awsbatch` and `healthomics` profiles need file
  inputs on S3.
- **`entry input '<name>' is or holds the run's work dir <dir>`**: on the
  `standard` and grid profiles every task reads a head-node input in place, so
  the input cannot hold the tasks' own directories. Pass a directory that does
  not hold the work directory, or launch with `-work-dir` outside the input.
- **`entry input '<name>': scheme '<scheme>' is not allowed`**: pass a path or
  an `s3://` URI, or `--mro2nf_allow_remote_inputs true`.
- **`entry input '<name>': bucket <bucket> is not mounted at /mnt/s3/<bucket>
  on this run's instances; mount it there, or pass the input as an s3:// URI,
  which the tasks stage if their role may read the bucket`**: on `awsbatch`, the input is a path under
  `/mnt/s3` whose bucket is not mounted. Deploy the stack with the bucket in
  `mountBuckets`, or pass its `s3://` URI on a compute environment whose
  instance role may read the bucket: the example stack's may read only the
  buckets it mounts.
- **`entry input '<name>' names no object under the mounted bucket
  <bucket>`**: on `awsbatch`, no object or prefix has the input's key.

### Containers and resources

- **`a container profile runs every task in params.mro2nf_image: pass
  --mro2nf_image <image>`**: a `docker`, `awsbatch` or `healthomics` run needs
  `--mro2nf_image`.
- **`Process requirement exceeds available memory`** (or `CPUs`): a task
  requests more than the local executor's pool, `executor.cpus` and
  `executor.memory`. Set `process.resourceLimits` no larger
  ([Resources](resources-and-retries.md#set-the-local-pool)).
- **`params.mro2nf_local_jobs grants each job from process.resourceLimits,
  which sets no cpus or memory`**: a `-c` config on the `standard` or `docker`
  profile removed the pool. Set both `cpus` and `memory`.
