# Parameters

A run takes two kinds of parameter: the pipeline's own inputs, and the
`--mro2nf_*` options that control how the run works.

## Pipeline inputs

Each input of the entry pipeline is a parameter of the same name. When it is
unset, the run uses the value in the `.mro`'s `call`.

The run's first task, PLAN, compiles the call with the launch values. So a
launch value changes forks, disabled calls and chunk counts exactly as the
same value written in the `.mro` would under `mrp`.

### How to pass a value

| Value | How | Example |
|---|---|---|
| `int`, `float`, `bool`, `string` | command line or `-params-file` | `--n 5 --scale 2.5 --skip false` |
| array, map, struct | typed JSON in a `-params-file` | `{"values": [5, 6, 7]}` |
| an input the top-level `map call` splits | Martian's split form in a `-params-file` | `{"x": {"split": [4, 5]}}` |
| `file`, `path`, a `filetype` | an absolute path or an `s3://` URI | `--reads /data/r1.fastq` |
| an input whose name starts with `_` | `--_name=value` (Nextflow reads only a name starting with a letter or digit in `--name value`) | `--_tag=x` |

Rules:

- A parameter the project does not declare, such as a misspelt input
  (`--fatcor 3`) or a stray key in a params file, fails the launch before any
  task starts: ``Parameter `fatcor` was specified on the command line or
  params file but is not declared in the script or config``.
- A command-line value must parse as the input's declared type, or PLAN fails,
  naming the input and its type.
- A params file's whole-number float for an `int` input (`{"n": 5.0}`) runs as
  the int, as `mrp` takes the literal `5.0`.
- A command-line value reaches the run as the text given: `--s 007` stays
  `007`. This needs Nextflow's default strict (v2) syntax parser; under
  `NXF_SYNTAX_PARSER=v1`, `--s 007` becomes the number `7`.
- An array, map or struct on the command line fails with `entry override of a
  non-scalar type cannot be a string; pass the value as typed JSON in a
  -params-file`.
- A split input takes `{"split": v}`: `{"x": {"split": [4, 5]}}` forks the
  call once per element, and `{"x": {"split": {"a": 4, "b": 5}}}` once per
  key, as `mrp` forks a call holding `split [4, 5]` or `split {"a": 4, "b":
  5}`. Any other value fails the run at PLAN, naming the input.
- File inputs have rules of their own, per profile:
  [Files and paths](files-and-paths.md).

> [!CAUTION]
> A JSON `-params-file` changes two kinds of number before mro2nf sees them.
> An int beyond int64 wraps: `{"n": 9223372036854775808}` runs as
> `-9223372036854775808`, where `mrp` refuses the literal. And `-0.0` becomes
> `0.0`. A YAML params file, a command-line value and an `.mro` literal keep
> both. Use a YAML params file, or keep values in range (the divergence
> [`params-json-int-wrap`](../reference/divergences.md)).

## The parameter schema

`nextflow_schema.json` describes every parameter in the
[nf-schema](https://nextflow-io.github.io/nf-schema/) format, for launch
forms and documentation. Each pipeline input sits at the schema's top level
with its `.mro` type and, as its default, its value in the `.mro`'s `call`:

| Martian type | Schema |
|---|---|
| `int`, `float`, `bool`, `string` | `integer`, `number`, `boolean`, `string` |
| `file`, a `filetype` | `string`, format `file-path` |
| `path` | `string`, format `path` (a file or a directory) |
| `T[]` | `array` of `T` |
| `map`, `map<T>` | `object`; a `map<T>`'s values are `T` |
| a `struct` | `object` with a property per member |
| `T`, split by the top-level `map call` | `object` with a required `split`: an `array` of `T` or an `object` of `T` |

The `mro2nf_*` parameters form the `mro2nf options` group. `job_resources`, a
map, sits at the top level, as nf-schema's meta-schema admits only scalar
types in a group. Those a profile sets are marked `hidden` and given no
default, as the profile picks their value.

### What is checked at launch

The schema validates nothing at launch: the project loads no nf-schema
plugin, which a run would have to download. Instead:

1. Nextflow checks that no undeclared parameter is set, since `main.nf`'s
   `params` block declares every one.
2. Nextflow checks the types of the `string` inputs and the `mro2nf_*`
   parameters, which the block declares.
3. PLAN checks every pipeline input against its declared Martian type, on
   every profile, and fails the run before any stage starts, naming the
   input, its type and the value (`entry input "f" declared float, got
   "2.5x"`).

The block leaves the other inputs untyped (`Object?`), because Nextflow's
types are not Martian's. On 26.04:

- an `Integer` refuses a params file's whole-number float (`{"n": 2.0}`),
  which `mrp` takes as the int `2`;
- a `Float` narrows a command-line value to 32 bits (`3.14159265358979`
  becomes `3.1415927`);
- an unset `Boolean` becomes `false` even with a `null` default, where the run
  takes the `.mro`'s value;
- a `Path` checks that an `s3://` input exists on the launch host.

## mro2nf options

| Parameter | Default | Effect |
|---|---|---|
| `--mro2nf_overrides <file>` | none | `mrp`'s `--overrides` file ([Resources and retries](resources-and-retries.md#mrp---overrides)) |
| `--mro2nf_monitor true` | `false` | `mrp`'s `--monitor`: `mrjob` kills a job that exceeds its memory request |
| `--mro2nf_confine true` | `false` | fails a stage task whose output resolves outside its work directory, instead of copying the file in; the container profiles always do this |
| `--mro2nf_image <image>` | none | the image every task runs in; required by the container profiles |
| `--mro2nf_mre <path>` | `mre` on `PATH` | the `mre` the tasks run |
| `--mro2nf_mrjob <path>` | the `mrjob` beside `mre` | the `mrjob` that runs `py` and `comp` stages |
| `--mro2nf_adapters <dir>` | the adapters `mre` embeds | Martian's adapters directory, holding `python/martian_shell.py` |
| `--mro2nf_psid <id>` | the `call`'s name | the pipestance ID `mrp` takes as its second argument; it names each job in `_jobinfo` |
| `--mro2nf_allow_remote_inputs true` | `false` | allows file inputs with URI schemes other than `s3://` |
| `--mro2nf_s3_mount <dir>` | `/mnt/s3` on `awsbatch`, none elsewhere | the directory under which every task's host may mount input buckets read-only, one directory per bucket ([Profiles](profiles.md#reading-s3-inputs-in-place)) |

`--mro2nf_mre`, `--mro2nf_mrjob` and `--mro2nf_adapters` let the stages run
under another Martian runtime, such as the one bundled with a pipeline.

A boolean option takes `true` or `false`, in any case, and alone
(`--mro2nf_monitor`) it means `true`. Nextflow refuses any other value at
launch, including a quoted `'true'` in a `-c` config.

### `job_resources`

`params.job_resources`, set in a `-c` config, maps a stage's `special` value to
the `clusterOptions` of its tasks on a grid executor, like `mrp`'s
`MRO_JOBRESOURCES`:

```groovy
params.job_resources = [highmem: '--partition=highmem']
```

[Inputs](../architecture/inputs.md) explains how launch values become the
run's invocation.
